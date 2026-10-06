package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const rawInspectLimit = 1 << 20

type rawDescription struct {
	Model     string `json:"model"`
	Operation string `json:"operation"`
	RequestID string `json:"requestId"`
}

type rawRequestChanges struct {
	Headers    map[string][]string `json:"headers"`
	BodyBase64 *string             `json:"bodyBase64"`
}

type rawCost struct {
	RequestID   string          `json:"requestId"`
	CostUSD     *float64        `json:"costUSD"`
	CostRequest *rawCostRequest `json:"costRequest"`
}

type rawCostRequest struct {
	URL     string              `json:"url"`
	Method  string              `json:"method"`
	Headers map[string][]string `json:"headers"`
}

func rawHookResult(value any, target any) error {
	encoded, err := common.Marshal(value)
	if err != nil {
		return err
	}
	return common.Unmarshal(encoded, target)
}

// rawHeaders excludes connection-only fields and every host authentication
// alias before the plugin applies its own provider credential.
func rawHeaders(headers http.Header, request bool) http.Header {
	out := headers.Clone()
	for _, line := range headers.Values("Connection") {
		for part := range strings.SplitSeq(line, ",") {
			out.Del(strings.TrimSpace(part))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		out.Del(name)
	}
	if request {
		for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "Mj-Api-Secret", "Cookie", "Sec-WebSocket-Protocol"} {
			out.Del(name)
		}
	}
	return out
}

func RelayRawPlugin(c *gin.Context) {
	if c.Request.Header.Get("Upgrade") != "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	plugin, route, exists := pluginruntime.DefaultRegistry.Generation().LookupRawRoute(c.Param("name"))
	if !exists {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	target, err := pluginruntime.RawTarget(route, c.Request.URL.EscapedPath(), c.Request.URL.RawQuery, c.Request.URL.ForceQuery)
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	// Only a hook requesting a replacement uses the captured body. No decoding,
	// defaulting, JSON serialization or model mapping occurs on the wire.
	limit := int64(max(1, constant.MaxFileDownloadMB)) << 20
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if int64(len(body)) > limit {
		c.AbortWithStatus(http.StatusRequestEntityTooLarge)
		return
	}
	request := map[string]any{"method": c.Request.Method, "path": target.EscapedPath(), "query": target.RawQuery, "headers": map[string][]string(rawHeaders(c.Request.Header, true)), "bodyBase64": base64.StdEncoding.EncodeToString(body)}
	hookCtx := map[string]any{"routeName": route.Name, "baseUrl": route.BaseURL}
	value, err := plugin.Engine.Call(c.Request.Context(), "describeRawRequest", hookCtx, request)
	var description rawDescription
	if err != nil || rawHookResult(value, &description) != nil || description.Operation != "submit" && description.Operation != "query" && description.Operation != "cancel" && description.Operation != "other" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if (description.Operation == "query" || description.Operation == "cancel") && description.RequestID == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	var record *model.RawProxyRequest
	if description.RequestID != "" {
		record, err = model.FindRawProxyRequest(plugin.Meta.Key, description.RequestID, c.GetInt("id"))
		if err != nil {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		description.Model = record.Model
	}
	if description.Model == "" || len(description.Model) > 255 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if c.GetBool("token_model_limit_enabled") {
		allowed, _ := c.Get("token_model_limit")
		limits, ok := allowed.(map[string]bool)
		if !ok || !limits[description.Model] {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
	}
	group := common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
	groups := []string{group}
	if group == "auto" {
		groups = service.GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup))
	}
	pinnedID := 0
	if pin, ok, _ := service.GetChannelConstraints(c).ResolvedPin(); ok {
		pinnedID = pin.ChannelId
	}
	if record != nil {
		if pinnedID != 0 && pinnedID != record.ChannelID {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		pinnedID = record.ChannelID
	}
	var channel *model.Channel
	for _, candidateGroup := range groups {
		channel, err = model.RawProxyChannel(plugin.Meta.Key, candidateGroup, pinnedID)
		if err == nil {
			group = candidateGroup
			break
		}
	}
	if channel == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	var key string
	var keyIndex int
	if record != nil {
		keyIndex = -1
		keys := channel.GetKeys()
		if !channel.ChannelInfo.IsMultiKey {
			keys = []string{channel.Key}
		}
		for index, candidate := range keys {
			if fmt.Sprintf("%x", sha256.Sum256([]byte(candidate))) == record.KeyHash {
				key, keyIndex = candidate, index
				break
			}
		}
		if keyIndex < 0 || (channel.ChannelInfo.IsMultiKey && channel.ChannelInfo.MultiKeyStatusList[keyIndex] != 0 && channel.ChannelInfo.MultiKeyStatusList[keyIndex] != common.ChannelStatusEnabled) {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
	} else {
		selectedKey, selectedIndex, apiErr := channel.GetNextEnabledKey()
		if apiErr != nil || selectedKey == "" {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		key, keyIndex = selectedKey, selectedIndex
	}

	if description.Operation == "submit" {
		balance, balanceErr := model.GetUserQuota(c.GetInt("id"), false)
		if balanceErr != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		var token model.Token
		tokenErr := model.DB.First(&token, c.GetInt("token_id")).Error
		if tokenErr != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
		if balance <= 0 || (!token.UnlimitedQuota && token.RemainQuota <= 0) {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
	}
	hookCtx = map[string]any{"routeName": route.Name, "baseUrl": route.BaseURL, "apiKey": key, "model": description.Model, "operation": description.Operation, "requestId": description.RequestID, "path": target.EscapedPath()}
	prepared, err := plugin.Engine.HasCallablePath(c.Request.Context(), "prepareRawRequest")
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	headers := rawHeaders(c.Request.Header, true)
	if prepared {
		value, err = plugin.Engine.Call(c.Request.Context(), "prepareRawRequest", hookCtx, request)
		var changes rawRequestChanges
		if err != nil || rawHookResult(value, &changes) != nil {
			c.AbortWithStatus(http.StatusBadGateway)
			return
		}
		for name, values := range changes.Headers {
			headers.Del(name)
			for _, value := range values {
				headers.Add(name, value)
			}
		}
		headers = rawHeaders(headers, false)
		if changes.BodyBase64 != nil {
			body, err = base64.StdEncoding.DecodeString(*changes.BodyBase64)
			if err != nil || int64(len(body)) > limit {
				c.AbortWithStatus(http.StatusBadGateway)
				return
			}
		}
	}
	if record == nil && description.Operation == "submit" {
		record = &model.RawProxyRequest{ID: uuid.NewString(), PluginKey: plugin.Meta.Key, ChannelID: channel.Id, UserID: c.GetInt("id"), TokenID: c.GetInt("token_id"), TokenName: c.GetString("token_name"), Model: description.Model, Group: group, KeyIndex: keyIndex, KeyHash: fmt.Sprintf("%x", sha256.Sum256([]byte(key))), CreatedAt: common.GetTimestamp()}
		if err := model.CreateRawProxyRequest(record); err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
	}
	client, err := service.RawHTTPClient(channel.GetSetting().Proxy, channel.GetSetting())
	if err != nil {
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	outbound, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		c.AbortWithStatus(http.StatusBadGateway)
		return
	}
	// Disallow transport-level replay too, including caller Idempotency-Key.
	// Empty bodies use a real reader so Transport cannot mark them replayable.
	outbound.GetBody = nil
	if outbound.Body == nil || outbound.Body == http.NoBody {
		outbound.Body = io.NopCloser(bytes.NewReader(body))
	}
	outbound.Header = headers
	outbound.Header.Del("Host")
	outbound.Header.Del("Content-Length")
	response, err := client.Do(outbound)
	if err != nil {
		status := http.StatusBadGateway
		if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			status = http.StatusGatewayTimeout
		}
		if record != nil {
			model.RawProxyAccountingError(record.ID, "transport_error")
		}
		c.AbortWithStatus(status)
		return
	}
	defer response.Body.Close()
	responseInfo := map[string]any{"statusCode": response.StatusCode, "headers": map[string][]string(response.Header.Clone()), "bodyText": "", "bodyTruncated": false}
	var prefix []byte
	mediaType := strings.ToLower(strings.SplitN(response.Header.Get("Content-Type"), ";", 2)[0])
	if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
		prefix, err = io.ReadAll(io.LimitReader(response.Body, rawInspectLimit+1))
		inspected := prefix
		encoding := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding")))
		inspectOK := len(prefix) <= rawInspectLimit
		if inspectOK && encoding == "gzip" {
			decoded, decodeErr := gzip.NewReader(bytes.NewReader(prefix))
			if decodeErr != nil {
				inspectOK = false
			} else {
				inspected, decodeErr = io.ReadAll(io.LimitReader(decoded, rawInspectLimit+1))
				decoded.Close()
				inspectOK = decodeErr == nil && len(inspected) <= rawInspectLimit
			}
		} else if encoding != "" && encoding != "identity" {
			inspectOK = false
		}
		if inspectOK && utf8.Valid(inspected) {
			responseInfo["bodyText"] = string(inspected)
		} else {
			responseInfo["bodyTruncated"] = true
		}
	}
	// Accounting failures must never replace a received provider response.
	if record != nil && !record.Billed {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 30*time.Second)
		err = accountRawResponse(ctx, c, plugin, hookCtx, responseInfo, record, client, route)
		cancel()
		if err != nil {
			model.RawProxyAccountingError(record.ID, "cost_unavailable")
			other := model.NewLogOther()
			other.SetAdmin("raw_request_id", record.ID)
			other.SetAdmin("accounting_error", "cost_unavailable")
			model.RecordErrorLog(c, record.UserID, record.ChannelID, record.Model, record.TokenName, "Raw provider cost unavailable", record.TokenID, 0, false, record.Group, other)
			common.SysError(fmt.Sprintf("raw accounting failed record=%s channel=%d", record.ID, record.ChannelID))
		}
	}
	for name, values := range rawHeaders(response.Header, false) {
		c.Writer.Header()[name] = slices.Clone(values)
	}
	c.Status(response.StatusCode)
	// Flush SSE chunks; binary bodies remain streamed without inspection.
	reader := io.MultiReader(bytes.NewReader(prefix), response.Body)
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			if _, writeErr := c.Writer.Write(buffer[:n]); writeErr != nil {
				return
			}
			if mediaType == "text/event-stream" {
				c.Writer.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

func accountRawResponse(ctx context.Context, c *gin.Context, plugin *pluginruntime.LoadedPlugin, hookCtx, response map[string]any, record *model.RawProxyRequest, client *http.Client, route pluginruntime.RawRoute) error {
	value, err := plugin.Engine.Call(ctx, "extractRawCost", hookCtx, response)
	var cost rawCost
	if err != nil {
		return err
	}
	if err := rawHookResult(value, &cost); err != nil {
		return err
	}
	if cost.RequestID != "" && record.UpstreamID == nil {
		if err := model.BindRawProxyRequest(record, cost.RequestID); err != nil {
			return err
		}
	}
	if cost.RequestID != "" && record.UpstreamID != nil && cost.RequestID != *record.UpstreamID {
		return fmt.Errorf("raw response request ID mismatch")
	}
	if cost.CostRequest != nil {
		descriptor := cost.CostRequest
		parsed, parseErr := url.Parse(descriptor.URL)
		if parseErr != nil || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || (strings.HasPrefix(route.BaseURL, "https://") && parsed.Scheme != "https") {
			return fmt.Errorf("invalid raw cost URL")
		}
		if descriptor.Method != "GET" || pluginruntime.ValidateRequestURL(descriptor.URL, route.BaseURL, plugin.Meta.AllowedHosts) != nil {
			return fmt.Errorf("invalid raw cost request")
		}
		quoteRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, descriptor.URL, nil)
		if err != nil {
			return fmt.Errorf("invalid raw cost URL")
		}
		quoteRequest.Header = rawHeaders(http.Header(descriptor.Headers), false)
		quoteResponse, err := client.Do(quoteRequest)
		if err != nil {
			return fmt.Errorf("raw cost request failed")
		}
		quoteBody, readErr := io.ReadAll(io.LimitReader(quoteResponse.Body, rawInspectLimit+1))
		quoteResponse.Body.Close()
		if readErr != nil || len(quoteBody) > rawInspectLimit {
			return fmt.Errorf("invalid raw cost response")
		}
		costResponse := map[string]any{"statusCode": quoteResponse.StatusCode, "headers": map[string][]string(quoteResponse.Header.Clone()), "bodyText": string(quoteBody)}
		value, err = plugin.Engine.Call(ctx, "extractRawCost", hookCtx, response, costResponse)
		if err != nil {
			return err
		}
		cost = rawCost{}
		if err := rawHookResult(value, &cost); err != nil {
			return err
		}
		if cost.CostRequest != nil {
			return fmt.Errorf("raw cost request recursion")
		}
	}
	if cost.RequestID != "" && record.UpstreamID != nil && cost.RequestID != *record.UpstreamID {
		return fmt.Errorf("raw pricing request ID mismatch")
	}
	if cost.CostUSD == nil {
		return nil
	}
	if record.UpstreamID == nil {
		return fmt.Errorf("raw final cost requires a request ID")
	}
	if math.IsNaN(*cost.CostUSD) || math.IsInf(*cost.CostUSD, 0) || *cost.CostUSD < 0 {
		return fmt.Errorf("invalid raw USD")
	}
	quota, clamp := common.QuotaRoundChecked(*cost.CostUSD * common.QuotaPerUnit)
	if clamp != nil {
		other := model.NewLogOther()
		other.SetAdmin("quota_saturation", clamp.AuditMap())
		other.SetAdmin("raw_request_id", record.ID)
		logger.LogWarn(c, "raw quota saturation record=%s", record.ID)
		model.RecordErrorLog(c, record.UserID, record.ChannelID, record.Model, record.TokenName, "Raw provider quota out of range", record.TokenID, 0, false, record.Group, other)
		return fmt.Errorf("raw cost quota out of range")
	}
	charged, err := model.SettleRawProxyRequest(record, *cost.CostUSD, quota)
	if err != nil || !charged {
		return err
	}
	other := model.NewLogOther()
	other.SetPublic("billing_mode", "raw_cost")
	other.SetPublic("cost_usd", *cost.CostUSD)
	other.SetPublic("raw_request_id", record.ID)
	if record.UpstreamID != nil {
		other.SetPublic("upstream_request_id", *record.UpstreamID)
	}
	model.RecordConsumeLog(c, record.UserID, model.RecordConsumeLogParams{ChannelId: record.ChannelID, ModelName: record.Model, TokenId: record.TokenID, TokenName: record.TokenName, Quota: quota, Group: record.Group, Content: "Raw provider request", Other: other})
	return nil
}

package controller

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	pluginruntime "github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func rawTestDB(t *testing.T, dialect gorm.Dialector, kind common.DatabaseType) *gorm.DB {
	t.Helper()
	oldDB, oldLog := model.DB, model.LOG_DB
	oldMain, oldLogType := common.MainDatabaseType(), common.LogDatabaseType()
	oldRedis, oldBatch := common.RedisEnabled, common.BatchUpdateEnabled
	common.RedisEnabled, common.BatchUpdateEnabled = false, false
	db, err := gorm.Open(dialect, &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(kind, kind)
	t.Cleanup(func() {
		sqlDB.Close()
		model.DB, model.LOG_DB = oldDB, oldLog
		common.SetDatabaseTypes(oldMain, oldLogType)
		common.RedisEnabled, common.BatchUpdateEnabled = oldRedis, oldBatch
	})
	require.NoError(t, db.Migrator().DropTable(&model.RawProxyRequest{}, &model.Log{}, &model.Token{}, &model.Channel{}, &model.User{}))
	// Representative deployed schema before raw: existing records survive upgrade.
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}))
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "raw-user", Group: "default", Quota: 100, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, db.Create(&model.Token{Id: 1, UserId: 1, Key: "new-api-test-token", Name: "raw-token", ExpiredTime: -1, RemainQuota: 100, Status: common.TokenStatusEnabled}).Error)
	settings := `{"task_plugin_key":"raw-test","raw_proxy_enabled":true}`
	base := "http://unused.invalid"
	priority := int64(20)
	require.NoError(t, db.Create(&model.Channel{Id: 7, Type: 61, Key: "provider-key", Name: "raw-channel", Group: "default", Models: "only-normalized-model", Priority: &priority, Status: common.ChannelStatusEnabled, BaseURL: &base, Setting: &settings}).Error)
	require.NoError(t, db.AutoMigrate(&model.RawProxyRequest{}))
	return db
}

const rawTestDriver = `
export const meta={apiVersion:1,key:"raw-test",name:"Raw Test",version:"1.0.0",author:{name:"Tests"},models:["only-normalized-model"],fetchMode:"per_task",rawRoutes:[{name:"test",baseUrl:BASE}],allowedHosts:[]};
export function buildSubmitRequest(){} export function parseSubmitResponse(){} export function parseTaskResult(){} export function buildQueryRequest(){}
export function describeRawRequest(ctx,r){const m=/\/requests\/([^/]+)/.exec(r.path);return {model:"unregistered/provider-model",operation:m?"query":(r.method==="POST"?"submit":"other"),requestId:m?m[1]:""};}
export function prepareRawRequest(ctx,r){return {headers:{Authorization:["Key "+ctx.apiKey]}};}
export function extractRawCost(ctx,r,price){const id=ctx.requestId||"job-1";if(r.statusCode<200||r.statusCode>=300)return {};if(!ctx.requestId&&ctx.routeName==="test"&&r.bodyText.includes("request_id"))return {requestId:id};const values=r.headers["X-Billable-Units"];if(!values)return {};if(!price)return {requestId:id,costRequest:{method:"GET",url:ctx.baseUrl+"/pricing",headers:{Authorization:["Key "+ctx.apiKey]}}};return {requestId:id,costUSD:Number(values[0])*JSON.parse(price.bodyText).rate};}
`

func rawTestRouter(t *testing.T, base string) *gin.Engine {
	t.Helper()
	return rawTestRouterSource(t, base, rawTestDriver)
}

func rawTestRouterSource(t *testing.T, base, driver string) *gin.Engine {
	t.Helper()
	old := pluginruntime.DefaultRegistry
	registry := pluginruntime.NewRegistry()
	source := strings.ReplaceAll(driver, "BASE", fmt.Sprintf("%q", base))
	_, err := registry.Register(source, pluginruntime.Options{})
	require.NoError(t, err)
	pluginruntime.DefaultRegistry = registry
	t.Cleanup(func() { pluginruntime.DefaultRegistry = old })
	service.InitHttpClient()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Any("/raw/:name/*path", func(c *gin.Context) {
		c.Set("id", 1)
		c.Set("token_id", 1)
		c.Set("token_name", "raw-token")
		c.Set("token_key", "new-api-test-token")
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		if c.GetHeader("Test-User") == "2" {
			c.Set("id", 2)
		}
		if c.GetHeader("Test-Limit") != "" {
			c.Set("token_model_limit_enabled", true)
			c.Set("token_model_limit", map[string]bool{"only-normalized-model": true})
		}
		RelayRawPlugin(c)
	})
	return r
}

func TestRawProxyPreservesWireAndAccountsOnce(t *testing.T) {
	db := rawTestDB(t, sqlite.Open(":memory:"), common.DatabaseTypeSQLite)
	var seenURI, seenAuth, seenEncoding string
	var seenBody []byte
	var quoteCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/pricing" {
			quoteCalls++
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"rate":0.02}`)
			return
		}
		seenURI, seenAuth, seenEncoding = r.RequestURI, r.Header.Get("Authorization"), r.Header.Get("Content-Encoding")
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Add("X-Provider-Trace", "a")
		w.Header().Add("X-Provider-Trace", "b")
		switch r.URL.Path {
		case "/submit/encoded/name":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			io.WriteString(w, `{"request_id":"job-1","response_url":"https://provider.example/result"}`)
		case "/requests/job-1":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Billable-Units", "3")
			w.Write([]byte{0, 255, 10, 13})
		case "/redirect":
			w.Header().Set("Location", "https://provider.example/next")
			w.WriteHeader(307)
			io.WriteString(w, "redirect body")
		default:
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(403)
			io.WriteString(w, "provider denied")
		}
	}))
	defer upstream.Close()
	r := rawTestRouter(t, upstream.URL)
	body := []byte("multipart opaque\x00\xff\r\n")
	req := httptest.NewRequest("POST", "/raw/test/submit/encoded%2Fname?x=%2F&x=2", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer new-api-test-token")
	req.Header.Set("Content-Type", "multipart/form-data; boundary=opaque")
	req.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, 202, w.Code)
	assert.Equal(t, "/submit/encoded%2Fname?x=%2F&x=2", seenURI)
	assert.Equal(t, body, seenBody)
	assert.Equal(t, "Key provider-key", seenAuth)
	assert.Equal(t, "gzip", seenEncoding)
	assert.Equal(t, []string{"a", "b"}, w.Header().Values("X-Provider-Trace"))
	assert.Contains(t, w.Body.String(), "https://provider.example/result")
	for range 2 {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/raw/test/requests/job-1", nil))
		require.Equal(t, 200, w.Code)
		assert.Equal(t, []byte{0, 255, 10, 13}, w.Body.Bytes())
	}
	var record model.RawProxyRequest
	require.NoError(t, db.First(&record).Error)
	assert.True(t, record.Billed)
	assert.InDelta(t, 0.06, record.CostUSD, 1e-10)
	expected, _ := common.QuotaRoundChecked(0.06 * common.QuotaPerUnit)
	var user model.User
	require.NoError(t, db.First(&user, 1).Error)
	assert.Equal(t, 100-expected, user.Quota)
	assert.Equal(t, expected, user.UsedQuota)
	assert.Equal(t, 1, user.RequestCount)
	assert.Equal(t, 1, quoteCalls)

	priority := int64(30)
	rawSettings := `{"task_plugin_key":"raw-test","raw_proxy_enabled":true}`
	require.NoError(t, db.Create(&model.Channel{Id: 8, Type: 61, Name: "higher priority", Key: "higher-key", Group: "default", Status: common.ChannelStatusEnabled, Priority: &priority, Setting: &rawSettings}).Error)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/raw/test/error", nil))
	assert.Equal(t, "Key higher-key", seenAuth)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/raw/test/requests/job-1", nil))
	assert.Equal(t, "Key provider-key", seenAuth)
	require.NoError(t, db.Delete(&model.Channel{}, 8).Error)
	// Existing jobs keep the original credential when channel keys are reordered.
	require.NoError(t, db.Model(&model.Channel{}).Where("id = 7").Updates(map[string]any{"key": "other-key\nprovider-key", "channel_info": model.ChannelInfo{IsMultiKey: true}}).Error)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/raw/test/requests/job-1", nil))
	require.Equal(t, 200, w.Code)
	assert.Equal(t, "Key provider-key", seenAuth)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = 7").Update("key", "removed-key").Error)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/raw/test/requests/job-1", nil))
	require.Equal(t, 503, w.Code)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = 7").Updates(map[string]any{"key": "provider-key", "channel_info": model.ChannelInfo{}}).Error)

	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{{"/redirect", 307, "redirect body"}, {"/error", 403, "provider denied"}} {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/raw/test"+tc.path, nil))
		assert.Equal(t, tc.status, w.Code)
		assert.Equal(t, tc.body, w.Body.String())
	}
	req = httptest.NewRequest("GET", "/raw/test/requests/job-1", nil)
	req.Header.Set("Test-User", "2")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, 404, w.Code)
	req = httptest.NewRequest("POST", "/raw/test/submit", strings.NewReader("{}"))
	req.Header.Set("Test-Limit", "enabled")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, 403, w.Code)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = 7").Update("setting", `{"task_plugin_key":"raw-test","raw_proxy_enabled":false}`).Error)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/raw/test/error", nil))
	assert.Equal(t, 503, w.Code)
}

func TestRawProxyLedgerDatabaseMatrix(t *testing.T) {
	databases := []struct {
		name    string
		kind    common.DatabaseType
		dialect gorm.Dialector
	}{{"sqlite", common.DatabaseTypeSQLite, sqlite.Open(t.TempDir() + "/raw.db")}}
	if dsn := os.Getenv("RAW_MYSQL_DSN"); dsn != "" {
		databases = append(databases, struct {
			name    string
			kind    common.DatabaseType
			dialect gorm.Dialector
		}{"mysql", common.DatabaseTypeMySQL, mysql.Open(dsn)})
	}
	if dsn := os.Getenv("RAW_POSTGRES_DSN"); dsn != "" {
		databases = append(databases, struct {
			name    string
			kind    common.DatabaseType
			dialect gorm.Dialector
		}{"postgres", common.DatabaseTypePostgreSQL, postgres.Open(dsn)})
	}
	for _, database := range databases {
		t.Run(database.name, func(t *testing.T) {
			db := rawTestDB(t, database.dialect, database.kind)
			if database.name != "sqlite" {
				sqlDB, _ := db.DB()
				sqlDB.SetMaxOpenConns(8)
			}
			var version string
			if database.name == "sqlite" {
				require.NoError(t, db.Raw("select sqlite_version()").Scan(&version).Error)
			} else {
				require.NoError(t, db.Raw("select version()").Scan(&version).Error)
			}
			t.Logf("database=%s version=%s", database.name, version)
			row := &model.RawProxyRequest{ID: "raw-ledger", PluginKey: "raw-test", ChannelID: 7, UserID: 1, TokenID: 1, Model: "unregistered/model"}
			require.NoError(t, model.CreateRawProxyRequest(row))
			require.NoError(t, model.BindRawProxyRequest(row, "external-job"))
			require.NoError(t, db.AutoMigrate(&model.RawProxyRequest{}))
			require.NoError(t, db.AutoMigrate(&model.RawProxyRequest{}))
			require.True(t, db.Migrator().HasIndex(&model.RawProxyRequest{}, "uk_raw_upstream"))
			var wg sync.WaitGroup
			errors := make(chan error, 8)
			charged := make(chan bool, 8)
			for range 8 {
				wg.Go(func() {
					copy := *row
					yes, err := model.SettleRawProxyRequest(&copy, 0.001, 500)
					errors <- err
					charged <- yes
				})
			}
			wg.Wait()
			close(errors)
			close(charged)
			for err := range errors {
				require.NoError(t, err)
			}
			count := 0
			for yes := range charged {
				if yes {
					count++
				}
			}
			assert.Equal(t, 1, count)
			// Close the database connection, reopen persisted storage, then replay.
			originalSQL, _ := db.DB()
			require.NoError(t, originalSQL.Close())
			reopened, openErr := gorm.Open(database.dialect, &gorm.Config{})
			require.NoError(t, openErr)
			reopenedSQL, _ := reopened.DB()
			reopenedSQL.SetMaxOpenConns(1)
			t.Cleanup(func() { reopenedSQL.Close() })
			db = reopened
			model.DB, model.LOG_DB = db, db
			restored, err := model.FindRawProxyRequest("raw-test", "external-job", 1)
			require.NoError(t, err)
			yes, err := model.SettleRawProxyRequest(restored, 0.001, 500)
			require.NoError(t, err)
			assert.False(t, yes)
			var user model.User
			var token model.Token
			var channel model.Channel
			require.NoError(t, db.First(&user, 1).Error)
			require.NoError(t, db.First(&token, 1).Error)
			require.NoError(t, db.First(&channel, 7).Error)
			assert.Equal(t, -400, user.Quota)
			assert.Equal(t, 500, user.UsedQuota)
			assert.Equal(t, 1, user.RequestCount)
			assert.Equal(t, -400, token.RemainQuota)
			assert.Equal(t, 500, token.UsedQuota)
			assert.Equal(t, int64(500), channel.UsedQuota)
			_, err = model.SettleRawProxyRequest(restored, -1, -1)
			assert.Error(t, err)
			_, err = model.FindRawProxyRequest("raw-test", "external-job", 2)
			assert.Error(t, err)

			zero := &model.RawProxyRequest{ID: "zero-ledger", PluginKey: "raw-test", ChannelID: 7, UserID: 1, TokenID: 1, Model: "free/model"}
			require.NoError(t, model.CreateRawProxyRequest(zero))
			require.NoError(t, model.BindRawProxyRequest(zero, "zero-job"))
			yes, err = model.SettleRawProxyRequest(zero, 0, 0)
			require.NoError(t, err)
			assert.True(t, yes)
			assert.True(t, zero.Billed)
			// Unique upstream IDs remain unique after two migrations.
			duplicate := *restored
			duplicate.ID = "duplicate"
			assert.Error(t, model.CreateRawProxyRequest(&duplicate))
		})
	}
}

func TestRawAuthenticationAfterPostpaidCharge(t *testing.T) {
	db := rawTestDB(t, sqlite.Open(":memory:"), common.DatabaseTypeSQLite)
	require.NoError(t, db.Model(&model.Token{}).Where("id = 1").Update("remain_quota", -10).Error)
	r := gin.New()
	r.GET("/raw/test/result", middleware.TokenAuth(), func(c *gin.Context) { c.Status(204) })
	req := httptest.NewRequest("GET", "/raw/test/result", nil)
	req.Header.Set("Authorization", "Bearer new-api-test-token")
	// Token parsing follows the New API convention: generated keys contain no hyphens.
	require.NoError(t, db.Model(&model.Token{}).Where("id = 1").Update("key", "rawtesttoken").Error)
	req.Header.Set("Authorization", "Bearer sk-rawtesttoken")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, 204, w.Code, w.Body.String())
	require.NoError(t, db.Model(&model.Token{}).Where("id = 1").Update("status", common.TokenStatusDisabled).Error)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, 401, w.Code)
}

func TestRawProxyTransportAndOptionalPrepare(t *testing.T) {
	db := rawTestDB(t, sqlite.Open(":memory:"), common.DatabaseTypeSQLite)
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	zw.Write([]byte(`{"native":true}`))
	require.NoError(t, zw.Close())
	var seen []byte
	var calls int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "unused.invalid", r.URL.Host)
		assert.Empty(t, r.Header.Get("Authorization"))
		seen, _ = io.ReadAll(r.Body)
		if r.URL.Path == "/reset" {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			conn.Close()
			return
		}
		if r.URL.Path == "/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: first\n\n")
			w.(http.Flusher).Flush()
			io.WriteString(w, "data: second\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(compressed.Bytes())
	}))
	defer proxy.Close()
	settings := fmt.Sprintf(`{"task_plugin_key":"raw-test","raw_proxy_enabled":true,"proxy":%q}`, proxy.URL)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = 7").Update("setting", settings).Error)
	driver := strings.Replace(rawTestDriver, `export function prepareRawRequest(ctx,r){return {headers:{Authorization:["Key "+ctx.apiKey]}};}`, "", 1)
	r := rawTestRouterSource(t, "http://unused.invalid", driver)
	for _, body := range [][]byte{[]byte(" { \"exact\": 1 } \n"), {0, 255, 10}} {
		req := httptest.NewRequest("PATCH", "/raw/test/native?x=%2F", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer local")
		req.Header.Set("X-Api-Key", "local")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		require.Equal(t, 200, w.Code)
		assert.Equal(t, body, seen)
		assert.Equal(t, compressed.Bytes(), w.Body.Bytes())
		assert.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/raw/test/events", nil))
	assert.Equal(t, "data: first\n\ndata: second\n\n", w.Body.String())
	assert.True(t, w.Flushed)
	assert.Equal(t, 3, calls)
	driver = strings.Replace(driver, `export function buildSubmitRequest(){}`, `export function buildSubmitRequest(){} export function prepareRawRequest(){return {bodyBase64:"AP8="};}`, 1)
	r = rawTestRouterSource(t, "http://unused.invalid", driver)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("PATCH", "/raw/test/native", strings.NewReader("original")))
	assert.Equal(t, []byte{0, 255}, seen)
	beforeReset := calls
	req := httptest.NewRequest("POST", "/raw/test/reset", strings.NewReader("{}"))
	req.Header.Set("Idempotency-Key", "fixture-key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, 502, w.Code)
	assert.Equal(t, beforeReset+1, calls, "raw POST must not be replayed after a reused connection fails")

}

func TestRawProxyAccountingFailurePreservesResponse(t *testing.T) {
	for _, scenario := range []string{"pricing_failed", "overflow", "negative"} {
		t.Run(scenario, func(t *testing.T) {
			db := rawTestDB(t, sqlite.Open(":memory:"), common.DatabaseTypeSQLite)
			native := `{"request_id":"job-1","result":"untouched"}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/pricing" {
					w.WriteHeader(503)
					io.WriteString(w, `{"error":"pricing unavailable"}`)
					return
				}
				io.WriteString(w, native)
			}))
			defer upstream.Close()
			driver := rawTestDriver[:strings.Index(rawTestDriver, "export function extractRawCost")]
			switch scenario {
			case "pricing_failed":
				driver += `export function extractRawCost(ctx,r,price){if(price)throw new Error("price failed");return {requestId:"job-1",costRequest:{method:"GET",url:ctx.baseUrl+"/pricing"}};}`
			case "overflow":
				driver += `export function extractRawCost(){return {requestId:"job-1",costUSD:1e100};}`
			case "negative":
				driver += `export function extractRawCost(){return {requestId:"job-1",costUSD:-1};}`
			}
			r := rawTestRouterSource(t, upstream.URL, driver)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("POST", "/raw/test/submit", strings.NewReader("{}")))
			assert.Equal(t, 200, w.Code)
			assert.Equal(t, native, w.Body.String())
			var record model.RawProxyRequest
			require.NoError(t, db.First(&record).Error)
			assert.False(t, record.Billed)
			assert.Equal(t, "cost_unavailable", record.AccountingError)
			var user model.User
			require.NoError(t, db.First(&user, 1).Error)
			assert.Equal(t, 100, user.Quota)
			if scenario == "overflow" {
				var logs []model.Log
				require.NoError(t, db.Find(&logs).Error)
				assert.True(t, len(logs) > 0)
				assert.Contains(t, logs[0].Other, "quota_saturation")
			}
		})
	}
}

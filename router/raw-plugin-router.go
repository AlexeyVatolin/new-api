package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

func SetRawPluginRouter(router *gin.Engine) {
	router.Any("/raw/:name/*path", middleware.RouteTag("relay"), middleware.SystemPerformanceCheck(), middleware.TokenAuth(), middleware.ModelRequestRateLimit(), controller.RelayRawPlugin)
}

package server

import (
	"github.com/PayU/redis-operator/controllers"
	"github.com/labstack/echo/v4"
)

func Register(e *echo.Echo) {
	e.GET("/state", clusterState)
	e.GET("/info", clusterInfo)
	e.GET("/hello", controllers.SayHello)
	e.GET("/reset", controllers.DoResetCluster)
	e.GET("/getconfigmap", controllers.GetConfigMap)
	e.GET("/createconfigmap", controllers.CreateConfigMap)
}

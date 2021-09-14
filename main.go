package main

import (
	"github.com/labstack/echo/v4"
	"log"
	"net/http"
)

func main() {
	e := echo.New()
	e.GET("/", func(context echo.Context) error {
		return context.String(http.StatusOK, "Hello")
	})

	err := e.Start(":80")
	if err != nil {
		log.Fatal(err)
	}
}

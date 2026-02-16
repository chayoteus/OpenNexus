package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/chayoteus/OpenNexus/internal/messenger"
	"github.com/chayoteus/OpenNexus/internal/ratelimit"
	"github.com/gin-gonic/gin"
)

func main() {
	r := gin.Default()

	// Limit request body to 64KB
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64*1024)
		c.Next()
	})

	// Per-IP rate limiting (configurable via RATE_LIMIT and RATE_BURST env vars)
	rateLimit := 100.0 // requests per second per IP
	if v := os.Getenv("RATE_LIMIT"); v != "" {
		if parsed, err := strconv.ParseFloat(v, 64); err == nil {
			rateLimit = parsed
		}
	}
	rateBurst := 200
	if v := os.Getenv("RATE_BURST"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			rateBurst = parsed
		}
	}
	if rateLimit > 0 {
		rl := ratelimit.New(rateLimit, rateBurst)
		r.Use(rl.Middleware())
		log.Printf("Rate limiting enabled: %.0f req/s, burst %d", rateLimit, rateBurst)
	} else {
		log.Println("Rate limiting disabled")
	}

	// Initialize handler (with or without Redis)
	var handler *messenger.Handler
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr != "" {
		redisPassword := os.Getenv("REDIS_PASSWORD")
		handler = messenger.NewWithRedis(redisAddr, redisPassword)
	} else {
		handler = messenger.New(nil)
	}

	// Routes
	r.GET("/health", handler.HealthCheck)
	r.GET("/info", handler.GetServerInfo)
	r.GET("/v1/stats/public", handler.GetPublicStats)
	r.OPTIONS("/v1/stats/public", handler.GetPublicStats)
	r.POST("/v1/presence/heartbeat", handler.PresenceHeartbeat)

	v1 := r.Group("/v1")
	{
		v1.POST("/messages", handler.SendMessage)
		v1.GET("/messages/stream", handler.StreamMessages)
		v1.GET("/messages/ws", handler.StreamMessagesWS)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	go func() {
		log.Printf("Messenger Server starting on port %s", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited")
}

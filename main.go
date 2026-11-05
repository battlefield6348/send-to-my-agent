package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	token := os.Getenv("API_TOKEN")
	if token == "" {
		log.Fatal("API_TOKEN is required: this API lets callers drive AI agents on this host")
	}
	agentName := os.Getenv("AGENT")
	agent, err := agentDefinitionFor(agentName)
	if err != nil {
		log.Fatal(err)
	}
	idleTimeout, err := durationFromEnv("IDLE_TIMEOUT", 10*time.Minute)
	if err != nil {
		log.Fatal(err)
	}
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	s := &server{
		token:    token,
		sessions: newSessionManager(idleTimeout, agent.start),
		timeout:  5 * time.Minute,
	}
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("agent %s listening on %s, idle sessions close after %s", agentName, addr, idleTimeout)
	log.Fatal(httpServer.ListenAndServe())
}

func durationFromEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration such as 10m; got %q", name, value)
	}
	return duration, nil
}

package main

import (
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
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	s := &server{
		token:         token,
		agentCommands: defaultAgentCommands,
		timeout:       5 * time.Minute,
	}
	log.Printf("listening on %s", addr)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(httpServer.ListenAndServe())
}

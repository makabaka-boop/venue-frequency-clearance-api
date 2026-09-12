package main

import (
	"log"
	"os"

	"wireless-coordinator/internal/api"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	if err := api.NewRouter().Run(":" + port); err != nil {
		log.Fatalf("server: %v", err)
	}
}

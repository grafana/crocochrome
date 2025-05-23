package main

import (
	"log"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) < 3 {
		log.Fatalf("Usage: %s <pid> <oom_score_adj>", os.Args[0])
	}

	pid := os.Args[1]
	newOOMScore := os.Args[2]

	adjFile, err := os.OpenFile(filepath.Join("/", "proc", pid, "oom_score_adj"), os.O_WRONLY, os.FileMode(0o600))
	if err != nil {
		log.Fatalf("Opening adjust file: %v", err)
	}

	defer func() {
		if err := adjFile.Close(); err != nil {
			log.Fatalf("Closing file: %v", err)
		}
	}()

	_, err = adjFile.Write([]byte(newOOMScore))
	if err != nil {
		log.Fatalf("Writing oom_score_adj: %v", err)
	}
}

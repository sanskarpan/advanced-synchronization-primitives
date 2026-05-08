package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/sanskar/syncprimitives/internal/primitives"
)

func main() {
	fmt.Println("=== Semaphore Example ===")

	// Create a semaphore with capacity 3 (max 3 concurrent workers)
	semaphore := primitives.NewSemaphore(3)

	var wg sync.WaitGroup
	numWorkers := 10

	fmt.Printf("Starting %d workers with semaphore capacity %d\n\n", numWorkers, semaphore.GetCapacity())

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			fmt.Printf("[Worker %d] Waiting to acquire semaphore...\n", id)
			semaphore.Acquire()

			fmt.Printf("[Worker %d] Acquired! Working... (Available: %d)\n", id, semaphore.GetCount())

			// Simulate work
			time.Sleep(500 * time.Millisecond)

			fmt.Printf("[Worker %d] Releasing semaphore\n", id)
			if err := semaphore.Release(); err != nil {
				fmt.Printf("[Worker %d] Release error: %v\n", id, err)
			}
		}(i)
	}

	wg.Wait()

	// Print statistics
	stats := semaphore.GetStats()
	fmt.Println("\n=== Semaphore Statistics ===")
	fmt.Println(stats.String())
}

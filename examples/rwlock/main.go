package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/sanskar/syncprimitives/internal/primitives"
)

func main() {
	fmt.Println("=== RWLock Example ===")

	rwlock := primitives.NewRWLock()

	sharedData := make(map[string]int)

	// Create multiple readers
	var wg sync.WaitGroup
	numReaders := 5
	numWriters := 2

	for i := 0; i < numReaders; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for j := 0; j < 3; j++ {
				rwlock.RLock()
				fmt.Printf("[Reader %d] Reading data: %v\n", id, sharedData)
				time.Sleep(100 * time.Millisecond)
				rwlock.RUnlock()

				time.Sleep(50 * time.Millisecond)
			}
		}(i)
	}

	// Create writers
	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for j := 0; j < 2; j++ {
				rwlock.Lock()
				key := fmt.Sprintf("writer%d", id)
				sharedData[key] = id*10 + j
				fmt.Printf("[Writer %d] Wrote: %s = %d\n", id, key, sharedData[key])
				time.Sleep(150 * time.Millisecond)
				rwlock.Unlock()

				time.Sleep(100 * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()

	// Print statistics
	stats := rwlock.GetStats()
	fmt.Println("\n=== RWLock Statistics ===")
	fmt.Println(stats.String())
}

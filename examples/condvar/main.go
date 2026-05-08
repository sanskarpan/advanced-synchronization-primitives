package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/sanskar/syncprimitives/internal/primitives"
)

func main() {
	fmt.Println("=== Condition Variable Example ===")

	mutex := primitives.NewMutex()
	condvar := primitives.NewCondVar()

	queue := make([]int, 0)
	var wg sync.WaitGroup

	// Producer goroutines
	numProducers := 3
	for i := 0; i < numProducers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for j := 0; j < 3; j++ {
				time.Sleep(200 * time.Millisecond)

				mutex.Lock()
				item := id*10 + j
				queue = append(queue, item)
				fmt.Printf("[Producer %d] Produced: %d (Queue size: %d)\n", id, item, len(queue))
				mutex.Unlock()

				// Signal one consumer
				condvar.Signal()
			}
		}(i)
	}

	// Consumer goroutines - balance consumption with production
	numConsumers := 2
	totalItems := numProducers * 3 // Each producer produces 3 items
	itemsPerConsumer := totalItems / numConsumers // Each consumer gets equal share

	for i := 0; i < numConsumers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for j := 0; j < itemsPerConsumer; j++ {
				mutex.Lock()

				// Wait for items in queue
				for len(queue) == 0 {
					fmt.Printf("[Consumer %d] Waiting for items...\n", id)
					condvar.Wait(mutex)
				}

				// Consume item
				item := queue[0]
				queue = queue[1:]
				fmt.Printf("[Consumer %d] Consumed: %d (Queue size: %d)\n", id, item, len(queue))

				mutex.Unlock()
			}
		}(i)
	}

	wg.Wait()

	// Print statistics
	cvStats := condvar.GetStats()
	mStats := mutex.GetStats()

	fmt.Println("\n=== Condition Variable Statistics ===")
	fmt.Println(cvStats.String())

	fmt.Println("\n=== Mutex Statistics ===")
	fmt.Println(mStats.String())
}

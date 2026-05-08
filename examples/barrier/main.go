package main

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/sanskar/syncprimitives/internal/primitives"
)

func main() {
	fmt.Println("=== Barrier Example ===")

	numWorkers := 5
	numPhases := 3

	barrier := primitives.NewBarrier(int32(numWorkers))

	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			for phase := 0; phase < numPhases; phase++ {
				// Simulate work in this phase
				workTime := time.Duration(rand.Intn(500)+100) * time.Millisecond
				fmt.Printf("[Worker %d] Phase %d: Working for %v\n", id, phase, workTime)
				time.Sleep(workTime)

				// Wait at barrier
				fmt.Printf("[Worker %d] Phase %d: Reached barrier (Arrived: %d/%d)\n",
					id, phase, barrier.GetArrived(), barrier.GetParties())

				arrivalIndex, _ := barrier.Wait()

				// All workers have reached the barrier
				if arrivalIndex == 0 {
					fmt.Printf("\n*** All workers completed phase %d ***\n\n", phase)
				}

				time.Sleep(100 * time.Millisecond) // Small delay before next phase
			}

			fmt.Printf("[Worker %d] All phases completed\n", id)
		}(i)
	}

	wg.Wait()

	// Print statistics
	stats := barrier.GetStats()
	fmt.Println("\n=== Barrier Statistics ===")
	fmt.Println(stats.String())
}

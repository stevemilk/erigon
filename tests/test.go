package main

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"
)

// Simulate a CPU-bound task
func cpuTask(taskID int) float64 {
	result := 0.0
	for i := 0; i < 1e6; i++ { // Heavy computation
		result += math.Sqrt(float64(i + taskID))
	}
	return result
}

func ioTask(taskID int) {
	time.Sleep(10 * time.Millisecond)
}

// Benchmark function for CPU-bound tasks
func cpuBenchmark(totalTasks int, numGoroutines int) time.Duration {
	tasks := make(chan int, totalTasks) // Channel to hold tasks
	var wg sync.WaitGroup

	// Start the specified number of goroutines
	start := time.Now()
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for taskID := range tasks {
				_ = cpuTask(taskID) // Process task
			}
		}()
	}

	// Add tasks to the channel
	for taskID := 0; taskID < totalTasks; taskID++ {
		tasks <- taskID
	}
	close(tasks) // Close the channel to signal no more tasks

	wg.Wait() // Wait for all goroutines to complete
	return time.Since(start)
}

func ioBenchmark(totalTasks int, numGoroutines int) time.Duration {
	tasks := make(chan int, totalTasks) // Channel to hold tasks
	var wg sync.WaitGroup

	// Start the specified number of goroutines
	start := time.Now()
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for taskID := range tasks {
				ioTask(taskID) // Process task
			}
		}()
	}

	// Add tasks to the channel
	for taskID := 0; taskID < totalTasks; taskID++ {
		tasks <- taskID
	}
	close(tasks) // Close the channel to signal no more tasks

	wg.Wait() // Wait for all goroutines to complete
	return time.Since(start)
}

/*
Benchmark results:

CPU Cores: 10
Tasks number: 1000

=== CPU-Bound Task Benchmark ===
1 goroutines: 335.22075ms
10 goroutines: 39.401667ms
100 goroutines: 38.951541ms
1000 goroutines: 39.360083ms
2000 goroutines: 40.008459ms

=== I/O-Bound Task Benchmark ===
1 goroutines: 10.976698875s
10 goroutines: 1.10276125s
100 goroutines: 109.47375ms
1000 goroutines: 12.514292ms
2000 goroutines: 12.588917ms
*/

func main() {
	fmt.Printf("CPU Cores: %d\n", runtime.NumCPU())
	fmt.Printf("Tasks number: %d\n", 1000)

	totalTasks := 1000                                // Fixed number of tasks
	goroutineConfigs := []int{1, 10, 100, 1000, 2000} // Different goroutine configurations

	fmt.Println("\n=== CPU-Bound Task Benchmark ===")
	for _, numGoroutines := range goroutineConfigs {
		duration := cpuBenchmark(totalTasks, numGoroutines)
		fmt.Printf("%d goroutines: %v\n", numGoroutines, duration)
	}

	fmt.Println("\n=== I/O-Bound Task Benchmark ===")
	for _, numGoroutines := range goroutineConfigs {
		duration := ioBenchmark(totalTasks, numGoroutines)
		fmt.Printf("%d goroutines: %v\n", numGoroutines, duration)
	}
}

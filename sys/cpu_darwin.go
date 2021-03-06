// Package sys provides methods to read system information
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package sys

// Containerized returns true if the application is running
// inside a container(docker/lxc/k8s)
func Containerized() bool {
	return false
}

// Returns the approximate number of CPUs allocated for the container.
func ContainerNumCPU() (int, error) {
	return 0, nil
}

// LoadAverage returns the system load average.
func LoadAverage() (avg LoadAvg, err error) {
	return
}

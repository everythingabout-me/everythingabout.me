// Package storageprobe contains isolated P0 capability experiments, not runtime
// drivers or a declaration that production write cutover is safe. Default tests
// use temporary native files and HTTP fixtures. Live COS tests require dedicated
// EAM_PROBE_COS_* configuration and never load application configuration or .env.
package storageprobe

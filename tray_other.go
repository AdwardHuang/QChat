//go:build !windows

package main

import "net/http"

func runServer(addr string, handler http.Handler) error {
	return http.ListenAndServe(addr, handler)
}

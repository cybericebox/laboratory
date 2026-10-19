//go:build !linux

package main

import "errors"

func run(string, string) error { return errors.New("headprog runs on linux only") }

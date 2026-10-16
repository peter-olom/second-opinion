//go:build !unix

package main

import "errors"

func syscallSocketpair() ([2]int, error) { return [2]int{}, errors.New("no socketpair") }

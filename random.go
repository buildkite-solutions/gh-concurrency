package main

import (
	cryptorand "crypto/rand"
	"math/big"
)

func cryptoIntn(max int) int {
	if max <= 0 {
		return 0
	}
	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0
	}
	return int(n.Int64())
}

type deterministicRand struct {
	state uint64
}

func newDeterministicRand(seed int64) *deterministicRand {
	return &deterministicRand{state: uint64(seed)}
}

func (r *deterministicRand) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *deterministicRand) Intn(max int) int {
	if max <= 0 {
		panic("invalid argument to Intn")
	}
	return int(r.next() % uint64(max))
}

func (r *deterministicRand) Shuffle(n int, swap func(i, j int)) {
	for i := n - 1; i > 0; i-- {
		swap(i, r.Intn(i+1))
	}
}

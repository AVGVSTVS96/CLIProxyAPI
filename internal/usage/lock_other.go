//go:build !unix

package usage

func lockState(string) (func(), error) { return func() {}, nil }

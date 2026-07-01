package main

type notFoundError struct {
	URL string
}

func (e notFoundError) Error() string {
	return "not found: " + e.URL
}

type authError struct{}

func (authError) Error() string {
	return "unauthorized"
}

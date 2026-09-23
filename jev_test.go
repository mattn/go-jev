package jev

import "testing"

func TestEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"localhost:8080":                       "http://localhost:8080/v1/systemone",
		"http://localhost:8080/":               "http://localhost:8080/v1/systemone",
		"https://example.com":                  "https://example.com/v1/systemone",
		"https://example.com/other":            "https://example.com/other",
		"example.com/other":                    "http://example.com/other",
		"https://api.typesafe.ai/v1/systemone": "https://api.typesafe.ai/v1/systemone",
	} {
		if got := Endpoint(in); got != want {
			t.Errorf("Endpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

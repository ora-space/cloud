package main

import "net/url"

// modelCredentialOrigin consumes configuration already validated by gateway.Config.
func modelCredentialOrigin(origin string) *url.URL {
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return nil
	}
	return u
}

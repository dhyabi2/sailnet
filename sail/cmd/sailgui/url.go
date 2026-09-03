package main

import "net/url"

func parseURL(s string) (*url.URL, error) { return url.Parse(s) }

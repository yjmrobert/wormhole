package main

import "embed"

//go:embed templates/*.html
var templates embed.FS

//go:embed assets
var assets embed.FS

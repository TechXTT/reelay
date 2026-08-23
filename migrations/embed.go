// Package migrations embeds the schema migration files.
//
// The embed directive has to live in this directory: go:embed cannot reach
// outside the package it appears in, and the spec keeps the .sql files at the
// repository root where they are easy to read without digging through Go
// packages. internal/store consumes FS.
package migrations

import "embed"

// FS holds every NNNN_name.sql file in this directory.
//
//go:embed *.sql
var FS embed.FS

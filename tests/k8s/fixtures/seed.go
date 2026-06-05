package fixtures

import (
	bifrostai "gopkg.volterra.us/go-bifrost-ai"
)

// AdminClient returns a typed go-bifrost-ai client aimed at the bifrost HTTP
// API (via the host port-forward). The API-sanity test uses it to create one
// of each governance entity.
func (bf *BifrostInstall) AdminClient() *bifrostai.Client {
	return bifrostai.NewClient(bf.BaseURL)
}

// i64ptr returns a pointer to an int64 literal (for optional SDK request fields).
func i64ptr(v int64) *int64 { return &v }

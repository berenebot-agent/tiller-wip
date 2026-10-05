// Package gateway is a thin, Chat-first HTTP client for Tiller and OpenRouter.
// It normalizes gateway catalogs, capability metadata, usage, safe errors and
// streaming fragments without importing Tiller internals. Request messages and
// response native fields retain their gateway wire shape for lossless replay.
// Applications own model selection, tool execution, schema validation and retries.
// No inference call requires catalog discovery first.
package gateway

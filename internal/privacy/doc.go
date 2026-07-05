// Package privacy implements the OpenViking PII redaction pipeline.
//
// The package ports the Python openviking/privacy/ module to Go. It
// provides:
//
//   - Extract: regex-based PII detection (email, phone, API key,
//     credit card, SSN-like). Patterns are intentionally conservative:
//     better to miss a PII than to over-redact.
//   - Substitute / Restore: bidirectional placeholder substitution.
//     Each PII value is replaced with a deterministic token such as
//     [EMAIL_1] or [PHONE_2]; Restore reverses the substitution using
//     a per-request PIIMap.
//   - Service.Redact / Service.Restore: the orchestrator methods used
//     by callers (bot agent loop, vectordb upsert path).
//   - SkillExtractor: a regex approximation of the Python
//     skill_extractor.py LLM-based extraction. The Python version
//     calls a VLM to identify sensitive values inside skill content;
//     the Go version uses the same PII regexes to find candidate
//     values inside skill blocks. This is a documented gap: semantic
//     field names from the LLM are not preserved.
//
// The PIIMap returned by Redact is per-request. Callers must NOT share
// it across requests; doing so leaks PII between requests.
//
// The package is free of external deps beyond the standard library so
// it can be imported from the bot, server, and CLI layers without
// pulling in heavy SDKs.
package privacy

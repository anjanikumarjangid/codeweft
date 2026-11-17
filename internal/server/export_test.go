package server

// SetLimits lowers the output limits for a test, so truncation can be
// exercised on the small fixture repository. It returns a function that
// restores the defaults.
func SetLimits(candidates, bodyLines, fileLinks int) (restore func()) {
	c, b, f := maxCandidates, maxBodyLines, maxFileLinks
	maxCandidates, maxBodyLines, maxFileLinks = candidates, bodyLines, fileLinks
	return func() { maxCandidates, maxBodyLines, maxFileLinks = c, b, f }
}

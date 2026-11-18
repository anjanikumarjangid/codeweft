package server

// SetLimits lowers the output limits for a test, so truncation can be
// exercised on the small fixture repository. It returns a function that
// restores the defaults.
func SetLimits(candidates, bodyLines, fileLinks int) (restore func()) {
	c, b, f := maxCandidates, maxBodyLines, maxFileLinks
	maxCandidates, maxBodyLines, maxFileLinks = candidates, bodyLines, fileLinks
	return func() { maxCandidates, maxBodyLines, maxFileLinks = c, b, f }
}

// SetMaxImpls lowers the get_implementations result limit for a test.
func SetMaxImpls(n int) (restore func()) {
	old := maxImpls
	maxImpls = n
	return func() { maxImpls = old }
}

// SetPageLimits lowers the get_file_summary and get_dependency_graph limits
// for a test.
func SetPageLimits(fileDecls, graphNodes, graphFan int) (restore func()) {
	d, n, f := maxFileDecls, maxGraphNodes, maxGraphFan
	maxFileDecls, maxGraphNodes, maxGraphFan = fileDecls, graphNodes, graphFan
	return func() { maxFileDecls, maxGraphNodes, maxGraphFan = d, n, f }
}

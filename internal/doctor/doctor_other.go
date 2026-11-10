//go:build !linux

package doctor

// Check: theLB runs only on Linux.
func Check(Facts) []Finding {
	return []Finding{{Area: "platform", Check: "operating system", State: Fail, Detail: "theLB runs only on Linux"}}
}

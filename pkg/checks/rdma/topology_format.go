package rdma

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"
)

var devTrailingNumberRe = regexp.MustCompile(`\d+`)

func splitDeviceNumber(dev string) (prefix, numStr, suffix string, ok bool) {
	loc := devTrailingNumberRe.FindStringSubmatchIndex(dev)
	if loc == nil {
		return "", "", "", false
	}
	return dev[:loc[0]], dev[loc[0]:loc[1]], dev[loc[1]:], true
}

// FormatPairsCompact renders GPU-NIC pairings in a compact human-readable form.
// Example: GPU0↔rdmap79s0-82s0 (×4), GPU1↔rdmap96s0-99s0 (×4) [multi_nic_pcie]
func FormatPairsCompact(pairs []checks.GPUNICPair, strategy checks.PairingStrategy) string {
	if len(pairs) == 0 {
		return ""
	}

	groups := make(map[int][]string)
	var order []int
	for _, p := range pairs {
		if _, ok := groups[p.GPU.ID]; !ok {
			order = append(order, p.GPU.ID)
		}
		groups[p.GPU.ID] = append(groups[p.GPU.ID], p.NIC.Dev)
	}
	sort.Ints(order)

	var parts []string
	for _, gpuID := range order {
		nics := groups[gpuID]
		desc := formatNICGroup(nics)
		if len(nics) > 1 {
			desc += fmt.Sprintf(" (×%d)", len(nics))
		}
		parts = append(parts, fmt.Sprintf("GPU%d↔%s", gpuID, desc))
	}

	out := strings.Join(parts, ", ")
	return out
}

func formatNICGroup(nics []string) string {
	if len(nics) == 0 {
		return ""
	}
	if len(nics) == 1 {
		return nics[0]
	}

	sort.Strings(nics)
	first := nics[0]
	last := nics[len(nics)-1]

	fp, fn, fs, ok1 := splitDeviceNumber(first)
	lp, ln, ls, ok2 := splitDeviceNumber(last)
	if ok1 && ok2 && fp == lp && fs == ls {
		firstNum, err1 := strconv.Atoi(fn)
		lastNum, err2 := strconv.Atoi(ln)
		if err1 == nil && err2 == nil && lastNum > firstNum {
			return fmt.Sprintf("%s%s%s-%s%s", fp, fn, fs, ln, ls)
		}
	}
	return strings.Join(nics, ",")
}

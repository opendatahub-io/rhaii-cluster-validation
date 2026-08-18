package crd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/opendatahub-io/rhaii-cluster-validation/pkg/checks"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/jsonpath"
)

// CRDSpec defines a CRD to check for.
//
// Two version dimensions are checked:
//
//  1. API version: the Kubernetes API version from spec.versions (e.g. "v1alpha2", "v1beta1",
//     "v1") — the version clients use in their manifests. Controlled by MinAPIVersion.
//
//  2. Release version: a semver string (e.g. "v1.4.0") extracted from CRD metadata
//     via a JSONPath expression. This tracks the project distribution/bundle version and
//     provides more granular information than the API version alone. Controlled by
//     ReleaseVersionJSONPath and MinReleaseVersion. Not all CRDs publish a release version.
type CRDSpec struct {
	// Full CRD name, e.g. "gateways.gateway.networking.k8s.io"
	Name string
	// Alternative CRD name to try if Name is not found (e.g. graduated API group)
	FallbackName string
	// Human-readable description for the report
	Description string
	// Remediation hint shown when the CRD is missing
	Remediation string
	// Minimum required served API version (e.g. "v1", "v1beta1"). Empty = any version.
	// This is the spec.versions[].name where served=true, not the release version.
	MinAPIVersion string
	// JSONPath expression to extract the release version from the CRD object.
	// Uses kubectl-style JSONPath syntax (e.g. "{.metadata.annotations.gateway\.networking\.k8s\.io/bundle-version}").
	// Empty means this CRD doesn't publish a release version.
	ReleaseVersionJSONPath string
	// Minimum required release version (semver, e.g. "v1.4.0"). Empty = report only, don't enforce.
	MinReleaseVersion string
}

// RequiredCRDs lists the CRDs required for llm-d deployment (design doc §5.2).
var RequiredCRDs = []CRDSpec{
	{
		Name:                   "gateways.gateway.networking.k8s.io",
		Description:            "Gateway API (Gateways)",
		Remediation:            "Install the Gateway API CRDs: kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/latest/download/standard-install.yaml",
		ReleaseVersionJSONPath: `{.metadata.annotations.gateway\.networking\.k8s\.io/bundle-version}`,
	},
	{
		Name:                   "httproutes.gateway.networking.k8s.io",
		Description:            "Gateway API (HTTPRoutes)",
		Remediation:            "Install the Gateway API CRDs: kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/latest/download/standard-install.yaml",
		ReleaseVersionJSONPath: `{.metadata.annotations.gateway\.networking\.k8s\.io/bundle-version}`,
	},
	{
		Name:        "inferencepools.inference.networking.x-k8s.io",
		Description: "InferencePool (Gateway API Inference Extension)",
		Remediation: "Install the Inference Extension CRDs: https://github.com/kubernetes-sigs/gateway-api-inference-extension#installation",
		// When the extension graduates, the CRD moves to inference.networking.k8s.io (no x- prefix).
		FallbackName:           "inferencepools.inference.networking.k8s.io",
		ReleaseVersionJSONPath: `{.metadata.annotations.inference\.networking\.k8s\.io/bundle-version}`,
	},
	{
		Name:        "leaderworkersets.leaderworkerset.x-k8s.io",
		Description: "LeaderWorkerSet",
		Remediation: "Install LeaderWorkerSet: https://github.com/kubernetes-sigs/lws#installation",
	},
	{
		Name:        "certificates.cert-manager.io",
		Description: "cert-manager",
		Remediation: "Install cert-manager: kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml",
	},
}

// Checker validates that required CRDs are installed in the cluster.
type Checker struct {
	client kubernetes.Interface
	crds   []CRDSpec
}

// NewChecker creates a CRD checker. If crds is nil, RequiredCRDs is used.
// minAPIVersions overlays API version requirements from platform config.
// minReleaseVersions overlays release version requirements from platform config.
func NewChecker(client kubernetes.Interface, crds []CRDSpec, minAPIVersions, minReleaseVersions map[string]string) *Checker {
	if len(crds) == 0 {
		crds = make([]CRDSpec, len(RequiredCRDs))
		copy(crds, RequiredCRDs)
	}
	for i := range crds {
		if v, ok := minAPIVersions[crds[i].Name]; ok && v != "" {
			crds[i].MinAPIVersion = v
		} else if crds[i].FallbackName != "" {
			if v, ok := minAPIVersions[crds[i].FallbackName]; ok && v != "" {
				crds[i].MinAPIVersion = v
			}
		}
		if v, ok := minReleaseVersions[crds[i].Name]; ok && v != "" {
			crds[i].MinReleaseVersion = v
		} else if crds[i].FallbackName != "" {
			if v, ok := minReleaseVersions[crds[i].FallbackName]; ok && v != "" {
				crds[i].MinReleaseVersion = v
			}
		}
	}
	return &Checker{client: client, crds: crds}
}

// Run checks each CRD and returns the results.
func (c *Checker) Run(ctx context.Context) []checks.Result {
	results := make([]checks.Result, 0, len(c.crds))
	for _, spec := range c.crds {
		results = append(results, c.checkCRD(ctx, spec))
	}
	return results
}

// fetchCRD tries to GET the CRD by spec.Name, falling back to spec.FallbackName
// if the primary name returns NotFound. Returns the raw JSON, the name that was
// found, and any error.
func (c *Checker) fetchCRD(ctx context.Context, spec CRDSpec) ([]byte, string, error) {
	raw, err := c.client.Discovery().RESTClient().Get().
		AbsPath("/apis/apiextensions.k8s.io/v1/customresourcedefinitions/" + spec.Name).
		Do(ctx).
		Raw()
	if err == nil {
		return raw, spec.Name, nil
	}
	if apierrors.IsNotFound(err) && spec.FallbackName != "" {
		fallbackRaw, fallbackErr := c.client.Discovery().RESTClient().Get().
			AbsPath("/apis/apiextensions.k8s.io/v1/customresourcedefinitions/" + spec.FallbackName).
			Do(ctx).
			Raw()
		if fallbackErr == nil {
			return fallbackRaw, spec.FallbackName, nil
		}
		if !apierrors.IsNotFound(fallbackErr) {
			return nil, spec.FallbackName, fallbackErr
		}
	}
	return nil, spec.Name, err
}

// crdVersionInfo is the subset of a CRD object we need for version checking.
type crdVersionInfo struct {
	Spec struct {
		Versions []struct {
			Name   string `json:"name"`
			Served bool   `json:"served"`
		} `json:"versions"`
	} `json:"spec"`
}

// checkCRD queries the API server for a specific CRD by name, discovers the
// highest served API version, optionally validates it against the minimum,
// and extracts the bundle/release version when a JSONPath is configured.
func (c *Checker) checkCRD(ctx context.Context, spec CRDSpec) checks.Result {
	r := checks.Result{
		Category: "crd",
		Name:     spec.Name,
	}

	raw, foundName, err := c.fetchCRD(ctx, spec)
	if err != nil {
		if apierrors.IsNotFound(err) {
			r.Status = checks.StatusFail
			r.Message = fmt.Sprintf("%s: not installed", spec.Description)
			r.Remediation = spec.Remediation
			return r
		}
		if apierrors.IsForbidden(err) {
			r.Status = checks.StatusWarn
			r.Message = fmt.Sprintf("%s: cannot check (insufficient permissions)", spec.Description)
			r.Remediation = "Grant 'get' permission on customresourcedefinitions (apiextensions.k8s.io) to the current user"
			return r
		}
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("%s: check failed: %v", spec.Description, err)
		return r
	}
	if foundName != spec.Name {
		r.Name = foundName
	}

	// Parse the CRD to discover served versions
	var crdInfo crdVersionInfo
	if jsonErr := json.Unmarshal(raw, &crdInfo); jsonErr != nil {
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("%s: installed but version check inconclusive (could not parse CRD response)", spec.Description)
		return r
	}

	var servedVersions []string
	bestVersion := ""
	for _, v := range crdInfo.Spec.Versions {
		if v.Served {
			servedVersions = append(servedVersions, v.Name)
			if bestVersion == "" || compareAPIVersions(v.Name, bestVersion) > 0 {
				bestVersion = v.Name
			}
		}
	}

	details := map[string]any{
		"served_versions": servedVersions,
	}

	if bestVersion == "" {
		r.Details = details
		r.Status = checks.StatusWarn
		r.Message = fmt.Sprintf("%s: installed but no served versions found", spec.Description)
		return r
	}

	// API version check
	r.Status = checks.StatusPass
	var apiMsg string
	if spec.MinAPIVersion == "" {
		apiMsg = fmt.Sprintf("%s installed (served: %s)", bestVersion, strings.Join(servedVersions, ", "))
	} else {
		details["min_api_version"] = spec.MinAPIVersion
		if compareAPIVersions(bestVersion, spec.MinAPIVersion) >= 0 {
			apiMsg = fmt.Sprintf("%s (>= %s, served: %s)", bestVersion, spec.MinAPIVersion, strings.Join(servedVersions, ", "))
		} else {
			r.Status = checks.StatusFail
			apiMsg = fmt.Sprintf("%s (requires API >= %s)", bestVersion, spec.MinAPIVersion)
			r.Remediation = fmt.Sprintf("Upgrade %s to API version >= %s", spec.Description, spec.MinAPIVersion)
		}
	}

	// Release version check (optional, per-CRD)
	releaseMsg := ""
	if spec.ReleaseVersionJSONPath != "" {
		releaseVer, extractErr := extractBundleVersion(raw, spec.ReleaseVersionJSONPath)
		if extractErr != nil || releaseVer == "" {
			// JSONPath configured but extraction failed or returned empty
			if spec.MinReleaseVersion != "" {
				details["min_release_version"] = spec.MinReleaseVersion
				r.Status = checks.StatusWarn
				if extractErr != nil {
					releaseMsg = fmt.Sprintf(", release version check inconclusive: %v", extractErr)
				} else {
					releaseMsg = ", release version not found in CRD metadata"
				}
			}
		} else {
			details["release_version"] = releaseVer
			if spec.MinReleaseVersion != "" {
				details["min_release_version"] = spec.MinReleaseVersion
				if compareSemver(releaseVer, spec.MinReleaseVersion) >= 0 {
					releaseMsg = fmt.Sprintf(", release %s (>= %s)", releaseVer, spec.MinReleaseVersion)
				} else {
					releaseMsg = fmt.Sprintf(", release %s (requires >= %s)", releaseVer, spec.MinReleaseVersion)
					r.Status = checks.StatusFail
					if r.Remediation == "" {
						r.Remediation = fmt.Sprintf("Upgrade %s to release version >= %s", spec.Description, spec.MinReleaseVersion)
					}
				}
			} else {
				releaseMsg = fmt.Sprintf(", release %s", releaseVer)
			}
		}
	}

	r.Details = details
	r.Message = fmt.Sprintf("%s: %s%s", spec.Description, apiMsg, releaseMsg)
	return r
}

// extractBundleVersion runs a JSONPath expression against the raw CRD JSON
// and returns the extracted string value. Returns ("", nil) when the path
// resolves but the field is absent (e.g. annotation not set on this CRD).
func extractBundleVersion(raw []byte, jsonPathExpr string) (string, error) {
	jp := jsonpath.New("bundleVersion")
	jp.AllowMissingKeys(true)
	if err := jp.Parse(jsonPathExpr); err != nil {
		return "", fmt.Errorf("invalid JSONPath %q: %w", jsonPathExpr, err)
	}

	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", fmt.Errorf("failed to unmarshal CRD: %w", err)
	}

	var buf bytes.Buffer
	if err := jp.Execute(&buf, data); err != nil {
		return "", err
	}

	return strings.TrimSpace(buf.String()), nil
}

// compareSemver compares two semver strings (e.g. "v1.4.0", "1.3.1", "v1.4.0-rc1").
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
// Pre-release versions sort before the corresponding release (v1.4.0-rc1 < v1.4.0).
func compareSemver(a, b string) int {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")

	aVer, aPre := splitPrerelease(a)
	bVer, bPre := splitPrerelease(b)

	aParts := strings.Split(aVer, ".")
	bParts := strings.Split(bVer, ".")

	maxLen := len(aParts)
	if len(bParts) > maxLen {
		maxLen = len(bParts)
	}

	for i := 0; i < maxLen; i++ {
		var aVal, bVal int
		if i < len(aParts) {
			aVal, _ = strconv.Atoi(aParts[i])
		}
		if i < len(bParts) {
			bVal, _ = strconv.Atoi(bParts[i])
		}
		if aVal != bVal {
			return cmpInt(aVal, bVal)
		}
	}

	// Numeric parts equal — a release (no pre-release) ranks above a pre-release
	if aPre == "" && bPre == "" {
		return 0
	}
	if aPre == "" {
		return 1
	}
	if bPre == "" {
		return -1
	}
	if aPre < bPre {
		return -1
	}
	if aPre > bPre {
		return 1
	}
	return 0
}

// splitPrerelease separates "1.4.0-rc1" into ("1.4.0", "rc1").
func splitPrerelease(v string) (version, prerelease string) {
	if idx := strings.Index(v, "-"); idx >= 0 {
		return v[:idx], v[idx+1:]
	}
	return v, ""
}

// apiVersion holds parsed components of a Kubernetes API version string.
type apiVersion struct {
	Major     int
	Stability int // 0=alpha, 1=beta, 2=GA
	Minor     int // e.g. the "2" in v1beta2 (0 for GA)
}

// parseAPIVersion parses strings like "v1", "v1beta1", "v2alpha3".
func parseAPIVersion(s string) apiVersion {
	s = strings.TrimPrefix(s, "v")

	av := apiVersion{Stability: 2} // assume GA

	if idx := strings.Index(s, "alpha"); idx >= 0 {
		av.Stability = 0
		av.Major, _ = strconv.Atoi(s[:idx])
		av.Minor, _ = strconv.Atoi(s[idx+5:])
		return av
	}
	if idx := strings.Index(s, "beta"); idx >= 0 {
		av.Stability = 1
		av.Major, _ = strconv.Atoi(s[:idx])
		av.Minor, _ = strconv.Atoi(s[idx+4:])
		return av
	}

	av.Major, _ = strconv.Atoi(s)
	return av
}

// compareAPIVersions compares two Kubernetes API version strings.
// Returns -1 if a < b, 0 if a == b, 1 if a > b.
// Ordering: v1alpha1 < v1alpha2 < v1beta1 < v1beta2 < v1 < v2alpha1 < ...
func compareAPIVersions(a, b string) int {
	av := parseAPIVersion(a)
	bv := parseAPIVersion(b)

	if av.Major != bv.Major {
		return cmpInt(av.Major, bv.Major)
	}
	if av.Stability != bv.Stability {
		return cmpInt(av.Stability, bv.Stability)
	}
	return cmpInt(av.Minor, bv.Minor)
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

package deploy

import _ "embed"

// DaemonSetYAML is the embedded agent DaemonSet manifest.
//
//go:embed daemonset.yaml
var DaemonSetYAML []byte

// RBACYAML is the embedded RBAC manifest (ServiceAccount, ClusterRole, ClusterRoleBinding).
//
//go:embed rbac.yaml
var RBACYAML []byte

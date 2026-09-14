// Package provenance is what a user can prove about the artifact they installed.
//
// A checksums file published on the same page as the artifact it checksums detects a
// corrupted download and detects nothing about anyone who could replace the artifact,
// because they could replace the checksums in the same breath. So each publishing
// workflow emits a signed, keyless build attestation (sigstore, CI OIDC, logged to a
// transparency log) binding the artifact digest to the commit, workflow and run that
// produced it. Nothing here signs anything: signing happens in CI, where the token
// exists. What lives here is the other half, the identity a verifier must pin to and the
// exact command that pins to it.
//
// That belongs in the product and not only in a doc page because a verification
// command is worth only as much as the identity it names, and the identity is a file
// path: rename the workflow and every published --signer-workflow line silently names a
// workflow that does not exist. Worse, dropping --signer-workflow still verifies, and
// accepts an attestation from any workflow in the repository. So the commands are
// generated from the same constants the workflows are named by.
//
// What this is not: attestation is not reproducibility (it says which run built the
// artifact, not that the same source rebuilds bit for bit), no release has been
// signed yet, and there is no dependency level provenance.
package provenance

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	Repo          = "DinethShakya23/kube-sre"
	OIDCIssuer    = "https://token.actions.githubusercontent.com"
	GHCRImage     = "ghcr.io/dinethshakya23/kube-sre"
	HelmChart     = "ghcr.io/dinethshakya23/charts/kube-sre"
	SPDXPredicate = "https://spdx.dev/Document"
)

// Signed is one class of released artifact and the workflow whose identity signs it.
type Signed struct {
	Key, What, Workflow, Command, Proves string
}

// Artifacts are the signed artifact classes. Commands take a {version} placeholder.
var Artifacts = []Signed{
	{
		Key: "image", What: "Container image — GHCR", Workflow: ".github/workflows/docker-publish.yml",
		Command: "gh attestation verify oci://" + GHCRImage + ":{version} --repo " + Repo + " \\\n" +
			"    --signer-workflow " + Repo + "/.github/workflows/docker-publish.yml\n" +
			"# and its SBOM, which is generated FROM the built image, not asserted about it:\n" +
			"gh attestation verify oci://" + GHCRImage + ":{version} --repo " + Repo + " \\\n" +
			"    --signer-workflow " + Repo + "/.github/workflows/docker-publish.yml \\\n" +
			"    --predicate-type " + SPDXPredicate,
		Proves: "this exact image digest was built by that workflow from that commit.",
	},
	{
		Key: "binaries", What: "kube-sre binaries attached to the release", Workflow: ".github/workflows/release-binaries.yml",
		Command: "gh attestation verify kube-sre_linux_amd64.tar.gz --repo " + Repo + " \\\n" +
			"    --signer-workflow " + Repo + "/.github/workflows/release-binaries.yml",
		Proves: "the tarball you downloaded is the one that build produced, which a checksums file cannot tell you, because it is published on the same page by the " +
			"same writer that would be replacing the tarball.",
	},
	{
		Key: "chart", What: "Helm chart — OCI, GHCR", Workflow: ".github/workflows/helm-publish.yml",
		Command: "gh attestation verify oci://" + HelmChart + ":{version} --repo " + Repo + " \\\n" +
			"    --signer-workflow " + Repo + "/.github/workflows/helm-publish.yml",
		Proves: "the chart digest a cluster is about to install came from that workflow. The chart is the enterprise install path, so leaving it unsigned would mean " +
			"signing everything except the thing most operators actually run.",
	},
}

// NotAttested are the channels that carry no attestation, and why.
var NotAttested = map[string]string{
	"go-install": "a `go install` build is compiled on the installer's machine from source they fetched, so there is no artifact of ours to attest; the module " +
		"proxy's checksum database is the verification path on that channel.",
}

// FirstAttestedTag is the first release that carries attestations, or "" while no
// release has been signed. It is the single place that says whether a release is signed.
const FirstAttestedTag = ""

// versionTuple turns v2.10.0 into [2 10 0]. It is numeric, because as strings "v2.10.0"
// sorts before "v2.9.0", which would report a signed release as unsigned for every
// minor version past the ninth.
func versionTuple(tag string) []int {
	core := strings.TrimPrefix(tag, "v")
	core, _, _ = strings.Cut(core, "-")
	core, _, _ = strings.Cut(core, "+")
	var out []int
	for _, p := range strings.Split(core, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

func lessTuple(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// AttestationExpected is whether a release at tag should carry attestations. It is
// false for every tag until the first signed release exists, and stays false for every
// tag published before it.
func AttestationExpected(tag string) bool { return attestationExpected(tag, FirstAttestedTag) }

func attestationExpected(tag, first string) bool {
	if first == "" {
		return false
	}
	return !lessTuple(versionTuple(tag), versionTuple(first))
}

// SignerIdentity is the certificate identity a release from tag will carry. The ref is
// part of it, so an attestation minted by the same workflow on a branch push cannot
// satisfy a check written for a tag.
func SignerIdentity(workflow, tag string) string {
	return fmt.Sprintf("https://github.com/%s/%s@refs/tags/%s", Repo, workflow, tag)
}

// SignerWorkflow is the --signer-workflow value: a path with no ref.
func SignerWorkflow(workflow string) string { return Repo + "/" + workflow }

// Entry is one artifact's verification command.
type Entry struct {
	Key, What, Workflow, Command, Proves, Identity string
}

// VerifyCommands are the exact commands a user runs to check one release. tag is the
// release tag (v1.2.3) and artifact references drop the leading v: a small difference
// and exactly the kind that makes a copied command fail for a reason nobody enjoys
// debugging, so it is applied once, here.
func VerifyCommands(tag string) []Entry {
	version := strings.TrimPrefix(tag, "v")
	out := make([]Entry, len(Artifacts))
	for i, a := range Artifacts {
		out[i] = Entry{a.Key, a.What, a.Workflow, strings.ReplaceAll(a.Command, "{version}", version), a.Proves, SignerIdentity(a.Workflow, tag)}
	}
	return out
}

// Report is the human readable guidance for verifying a release. It says whether the
// release is signed from data and not by assertion: opening with a claim of a signature
// while no release had ever been attested, then printing commands that could only fail,
// hands the user the alarming reading of "never signed" versus "signature missing".
func Report(tag string) string {
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n  Verifying a kube-sre release — %s\n\n", tag)
	if AttestationExpected(tag) {
		fmt.Fprintf(&b, "  Each artifact carries a keyless sigstore attestation minted by the workflow\n  that built it, under %s.\n", OIDCIssuer)
	} else {
		fmt.Fprintf(&b, "  %s is not signed — it carries no attestation of any kind.\n", tag)
		if FirstAttestedTag == "" {
			b.WriteString("  No release has been signed yet: the attest steps run on the first tag cut\n  after they were added, so they have never run.\n")
		} else {
			fmt.Fprintf(&b, "  Signing began at %s; this tag predates it.\n", FirstAttestedTag)
		}
		b.WriteString("  The commands below will therefore fail to find an attestation for this\n  tag. That failure is expected here and is NOT evidence of tampering,\n  which is the one reading of a failed verification that should alarm you.\n")
		fmt.Fprintf(&b, "\n  The identities they pin are under %s.\n", OIDCIssuer)
	}
	b.WriteString("  Nothing below runs here: these are the commands YOU run, on the machine that\n  pulled the artifact. `gh attestation verify` needs gh >= 2.49.\n\n")
	for _, e := range VerifyCommands(tag) {
		fmt.Fprintf(&b, "  %s\n", e.What)
		for _, ln := range strings.Split(e.Command, "\n") {
			fmt.Fprintf(&b, "    %s\n", ln)
		}
		fmt.Fprintf(&b, "    → proves: %s\n    → signer: %s\n\n", e.Proves, e.Identity)
	}
	b.WriteString("  Not attested, and why:\n")
	for ch, why := range NotAttested {
		fmt.Fprintf(&b, "    • %s: %s\n", ch, why)
	}
	b.WriteString("\n  A signed provenance says which run built this, from which commit. It does\n  NOT say the same source rebuilds bit-for-bit — these are not reproducible builds.\n\n")
	return b.String()
}

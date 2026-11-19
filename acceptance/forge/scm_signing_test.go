//go:build acceptance

package forge

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"github.com/updatecli/updatecli/acceptance/internal/cli"
)

func testSCMSignedCommit(t *testing.T, s *server) {
	r := s.newRepo(t)
	const author, email = "Acceptance Signer", "scm-signer@example.invalid"
	const id, file, content = "signed-commit", "signed.txt", "content from the signed update\n"
	key, err := openpgp.NewEntity(author, "", email, nil)
	require.NoError(t, err)
	passphrase := rand.Text()
	require.NoError(t, key.EncryptPrivateKeys([]byte(passphrase), nil))
	var private, public bytes.Buffer
	privWriter, err := armor.Encode(&private, openpgp.PrivateKeyType, nil)
	require.NoError(t, err)
	require.NoError(t, key.SerializePrivateWithoutSigning(privWriter, nil))
	require.NoError(t, privWriter.Close())
	pubWriter, err := armor.Encode(&public, openpgp.PublicKeyType, nil)
	require.NoError(t, err)
	require.NoError(t, key.Serialize(pubWriter))
	require.NoError(t, pubWriter.Close())

	// Keep the disposable secret out of the YAML and never report CLI output.
	c := cli.New(t, "UPDATECLI_GITEA_USERNAME="+s.owner, "UPDATECLI_GITEA_TOKEN="+s.token,
		"FORGE_SCM_SIGNING_KEY="+private.String(), "FORGE_SCM_SIGNING_PASSPHRASE="+passphrase)
	spec := scmTestSpec(r)
	spec["user"], spec["email"] = author, email
	spec["commitmessage"] = map[string]any{"type": "fix", "scope": "signing", "body": "Verify the published commit.", "hidecredit": true}
	spec["gpg"] = map[string]string{
		"signingkey": "acceptance-signing-key",
		"passphrase": `{{ requiredEnv "FORGE_SCM_SIGNING_PASSPHRASE" }}`,
	}
	// Insert an expression that renders a quoted YAML scalar, preserving the
	// armor's newlines. Marshaling the expression as a string would quote it twice.
	manifest := strings.Replace(scmFileManifest(t, spec, id, file, content),
		"signingkey: acceptance-signing-key", `signingkey: {{ printf "%q" (requiredEnv "FORGE_SCM_SIGNING_KEY") }}`, 1)
	before := r.refs(t)
	out := c.Pipeline(t, "apply", manifest)
	require.Zero(t, out.ExitCode, "publishing a signed SCM commit must succeed")

	workingRef := "refs/heads/updatecli_" + r.base + "_" + id
	after := r.refs(t)
	assertSCMRefChange(t, before, after, workingRef)
	tip := after[workingRef]
	r.git(t, "fetch", "origin", workingRef)
	base := before["refs/heads/"+r.base]
	require.Equal(t, base+"\n", r.git(t, "show", "-s", "--format=%P", tip))
	require.Equal(t, content, r.git(t, "show", tip+":"+file))
	require.Equal(t, author+" <"+email+">|fix(signing): Write "+file+"\n\nVerify the published commit.\n",
		r.git(t, "show", "-s", "--format=%an <%ae>|%B", tip))

	// Read the pushed object via native Git and verify its bytes, not a forge
	// badge or a header that could contain an arbitrary signature.
	readCommit := func(sha string) *object.Commit {
		t.Helper()
		raw := new(plumbing.MemoryObject)
		raw.SetType(plumbing.CommitObject)
		_, err := raw.Write([]byte(r.git(t, "cat-file", "commit", sha)))
		require.NoError(t, err)
		commit, err := object.DecodeCommit(nil, raw)
		require.NoError(t, err)
		require.Equal(t, sha, commit.Hash.String())
		return commit
	}
	_, err = readCommit(base).Verify(public.String())
	require.Error(t, err, "the unsigned fixture commit must fail verification")
	signer, err := readCommit(tip).Verify(public.String())
	require.NoError(t, err, "unsigned commits and signatures from any other key must fail verification")
	require.Equal(t, key.PrimaryKey.Fingerprint, signer.PrimaryKey.Fingerprint)
}

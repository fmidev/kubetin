// Trust prompt + -trust subcommand.
//
// runTrust adds every currently-discovered kubeconfig to the trust
// list. It's the non-interactive escape hatch the README points at:
// `kubetin -trust` once after install (or after intentionally adding
// a new kubeconfig), then `kubetin` works as before.
//
// loadTrustedDiscovery is the normal startup path. Three branches:
//
//  1. trust list is non-empty AND every discovered file is in it:
//     return the trusted Discovered, no prompts.
//
//  2. trust list is empty (first ever run): show a one-time prompt
//     listing the discovered files. y → bless all + persist + carry
//     on, n → exit. We allow this on first run because there's no
//     "previous baseline" to compare against and forcing the user to
//     bounce back to a docs page is hostile.
//
//  3. trust list exists but discovered files don't all match: refuse
//     to load the unknown ones and tell the user how to bless them.
//     We deliberately don't prompt here — files appearing silently
//     between sessions is exactly the threat the trust list defends
//     against, and an interactive y/N with the malicious payload
//     already loaded into client-go is too late.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/fmidev/kubetin/internal/kubeconfig"
)

func runTrust() error {
	files, err := kubeconfig.CandidateFilesForTrust()
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no kubeconfig files found to trust")
	}
	tl, _ := kubeconfig.LoadTrustList()
	for _, f := range files {
		if err := tl.Add(f); err != nil {
			fmt.Fprintf(os.Stderr, "  skipping %s: %v\n", f, err)
			continue
		}
	}
	if err := tl.Save(); err != nil {
		return fmt.Errorf("save trust list: %w", err)
	}
	fmt.Printf("kubetin: trusted %d kubeconfig file(s); list saved to %s\n",
		len(files), tl.Path())
	return nil
}

func loadTrustedDiscovery() (*kubeconfig.Discovered, error) {
	tl, loadErr := kubeconfig.LoadTrustList()
	if loadErr != nil && tl.Existed() {
		// File is on disk but we couldn't parse it (permissions,
		// truncation, bad encoding, …). DO NOT fall through into the
		// first-run interactive bless — the user would type 'y' to a
		// prompt that lies about being "your first run", and Save()
		// would O_TRUNC over the existing list. Fail closed and tell
		// the user what to inspect.
		return nil, fmt.Errorf(
			"trust list at %s exists but is unreadable: %v\n"+
				"Refusing to start — inspect or remove the file and retry. "+
				"If you intend to start fresh, `rm` it and rerun `kubetin -trust`.",
			tl.Path(), loadErr)
	}
	if loadErr != nil {
		// File doesn't exist (or path couldn't be resolved). Path
		// errors get a one-line note and we proceed as first-run.
		fmt.Fprintf(os.Stderr, "kubetin: trust list path unresolved (%v); treating as first run\n", loadErr)
	}

	d, untrusted, err := kubeconfig.DiscoverTrusted(tl)
	if err != nil {
		return nil, err
	}

	if len(untrusted) == 0 {
		return d, nil
	}

	if !tl.Existed() {
		// True first run — no file ever existed. Interactive bless.
		ok, err := promptTrustAll(untrusted)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("aborted at trust prompt")
		}
		for _, f := range untrusted {
			_ = tl.Add(f)
		}
		if err := tl.Save(); err != nil {
			return nil, fmt.Errorf("save trust list: %w", err)
		}
		// Re-run discovery now that everything's trusted. Cheaper than
		// merging two partial Discovereds.
		d, _, err = kubeconfig.DiscoverTrusted(tl)
		return d, err
	}

	// Non-first-run: refuse the untrusted set, but distinguish two
	// cases. "Content changed" — a path we previously trusted now
	// hashes differently — is overwhelmingly common after `oc login`,
	// `gcloud container clusters get-credentials`, or kubelogin
	// refresh. "New file" is the original threat model.
	var changed, added []string
	for _, f := range untrusted {
		if tl.HasKnownPath(f) {
			changed = append(changed, f)
		} else {
			added = append(added, f)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "kubetin: refusing %d untrusted kubeconfig file(s):\n", len(untrusted))
	if len(changed) > 0 {
		fmt.Fprintf(&b, "\n  Content changed (e.g. `oc login` / `gcloud …` rotated the token):\n")
		for _, f := range changed {
			fmt.Fprintf(&b, "    %s\n", f)
		}
	}
	if len(added) > 0 {
		fmt.Fprintf(&b, "\n  Newly appeared (not previously trusted):\n")
		for _, f := range added {
			fmt.Fprintf(&b, "    %s\n", f)
		}
	}
	fmt.Fprintf(&b, "\nRun `kubetin -trust` to re-bless these files.\n")

	if len(d.Contexts) == 0 {
		// Nothing trusted at all — full stop.
		return nil, fmt.Errorf("%s", b.String())
	}
	// Some files are trusted; warn about the rest and continue.
	fmt.Fprint(os.Stderr, b.String())
	return d, nil
}

func promptTrustAll(files []string) (bool, error) {
	fmt.Println("kubetin: this is your first run. The following kubeconfig files were discovered:")
	for _, f := range files {
		fmt.Printf("  %s\n", f)
	}
	fmt.Println()
	fmt.Println("Each kubeconfig may invoke external auth plugins (gke-gcloud-auth-plugin,")
	fmt.Println("aws-iam-auth, kubelogin, …) at connect time. Only add files you trust.")
	fmt.Print("Trust all of the above and continue? [y/N] ")
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

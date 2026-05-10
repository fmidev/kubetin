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
//  3. trust list exists but discovered files don't all match: prompt
//     the user, same shape as first-run, distinguishing "content
//     changed" (a path we previously trusted now hashes differently
//     — typical after `oc login` / `gcloud … get-credentials` /
//     kubelogin refresh) from "newly appeared" (the original threat
//     model). y → re-bless the listed files + persist + carry on,
//     n → exit. We prompt rather than warn-and-continue because the
//     bubbletea alt-screen wipes anything printed to stderr the
//     instant the TUI starts, so a warning the user never sees is
//     equivalent to no warning at all. The prompt is safe: untrusted
//     files have NOT been loaded into client-go at this point — they
//     only get loaded after re-discovery, after the user accepts.
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

	// Non-first-run: trust list exists, but some discovered files are
	// unknown or have changed. Prompt — the previous behaviour of
	// warn-and-continue was equivalent to silent failure because the
	// TUI's alt-screen erases stderr the moment it starts.
	var changed, added []string
	for _, f := range untrusted {
		if tl.HasKnownPath(f) {
			changed = append(changed, f)
		} else {
			added = append(added, f)
		}
	}

	ok, err := promptTrustChanges(changed, added)
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
	d, _, err = kubeconfig.DiscoverTrusted(tl)
	return d, err
}

func promptTrustAll(files []string) (bool, error) {
	fmt.Println("kubetin: this is your first run. The following kubeconfig files were discovered:")
	for _, f := range files {
		fmt.Printf("  %s\n", f)
	}
	fmt.Println()
	fmt.Println("Each kubeconfig may invoke external auth plugins (gke-gcloud-auth-plugin,")
	fmt.Println("aws-iam-auth, kubelogin, …) at connect time. Only add files you trust.")
	return askYesNo("Trust all of the above and continue? [y/N] ")
}

// promptTrustChanges runs the same shape of prompt as promptTrustAll,
// but for the case where the trust list already exists and some
// discovered files no longer match it. Sections are emitted only when
// non-empty so the prompt stays scannable when only one bucket applies
// (e.g. just an `oc login` token rotation).
func promptTrustChanges(changed, added []string) (bool, error) {
	fmt.Println("kubetin: kubeconfig trust list has unverified entries.")
	if len(changed) > 0 {
		fmt.Println()
		fmt.Println("  Content changed (e.g. `oc login` / `gcloud …` / kubelogin rotated a token):")
		for _, f := range changed {
			fmt.Printf("    %s\n", f)
		}
	}
	if len(added) > 0 {
		fmt.Println()
		fmt.Println("  Newly appeared (not previously trusted):")
		for _, f := range added {
			fmt.Printf("    %s\n", f)
		}
	}
	fmt.Println()
	fmt.Println("Each kubeconfig may invoke external auth plugins at connect time.")
	fmt.Println("Only re-trust if you recognise the change.")
	return askYesNo("Re-trust all of the above and continue? [y/N] ")
}

func askYesNo(prompt string) (bool, error) {
	fmt.Print(prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

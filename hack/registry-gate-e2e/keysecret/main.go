// Command keysecret makes the pull Secret of a lab on the image cache the way the platform does: the raw
// dockerconfigjson that the management agent writes from the registry credentials of CreateLabs (one entry
// per registry host), run through the operator's own re-keying (imagecache.RekeyForCache): one key per
// registry under the cache path, the tenant in the username. It prints the document; with -raw it prints
// the raw one instead (what a pod must not be given).
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/cybericebox/laboratory/internal/imagecache"
)

func main() {
	prefix := flag.String("prefix", "localhost:5035", "the cache as the nodes see it")
	tenant := flag.String("tenant", "", "the tenant label of the lab")
	served := flag.String("served", "", "comma separated registries the cache serves")
	registry := flag.String("registry", "", "the registry host of the credential")
	user := flag.String("user", "", "the upstream username")
	pass := flag.String("pass", "", "its password")
	raw := flag.Bool("raw", false, "print the raw document, not the re-keyed one")
	flag.Parse()
	doc, err := json.Marshal(map[string]any{"auths": map[string]any{strings.ToLower(*registry): map[string]string{
		"username": *user, "password": *pass, "auth": base64.StdEncoding.EncodeToString([]byte(*user + ":" + *pass)),
	}}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *raw {
		fmt.Print(string(doc))
		return
	}
	out, n, err := imagecache.RekeyForCache(*prefix, *tenant, strings.Split(*served, ","), doc)
	if err != nil || n == 0 {
		fmt.Fprintln(os.Stderr, "nothing to write:", err)
		os.Exit(1)
	}
	fmt.Print(string(out))
}

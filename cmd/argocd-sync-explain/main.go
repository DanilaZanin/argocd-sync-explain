// Command argocd-sync-explain explains what an Argo CD Application is waiting
// for, or why it failed or is out of sync. It is read-only.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/DanilaZanin/argocd-sync-explain/internal/collect"
	"github.com/DanilaZanin/argocd-sync-explain/internal/explain"
	"github.com/DanilaZanin/argocd-sync-explain/internal/render"
)

// version is set by goreleaser.
var version = "dev"

const usageText = `Explain what an Argo CD Application is waiting for, or why it failed or is out of sync.
Read-only: nothing is changed.

Usage:
  %[1]s app NAME [flags]

Flags:
`

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv)) }

func run(args []string, stdout, stderr *os.File, env func(string) string) int {
	prog := filepath.Base(os.Args[0])
	if strings.HasPrefix(prog, "kubectl-") {
		prog = "kubectl argocd-sync-explain"
	}
	fs := pflag.NewFlagSet(prog, pflag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		namespace   = fs.StringP("namespace", "n", "argocd", "namespace of the Application")
		argoNS      = fs.String("argocd-namespace", "", "namespace of the Argo CD installation (default: same as --namespace)")
		output      = fs.StringP("output", "o", "text", "output format: text or json")
		mode        = fs.String("mode", "auto", "auto, api or kubeconfig (auto: api when a server or --port-forward is given, else kubeconfig)")
		server      = fs.String("server", "", "Argo CD server address (env ARGOCD_SERVER, or the argocd CLI config)")
		token       = fs.String("auth-token", "", "Argo CD auth token (env ARGOCD_AUTH_TOKEN, or the argocd CLI config)")
		insecure    = fs.Bool("insecure", false, "skip TLS verification of the Argo CD server")
		plainText   = fs.Bool("plaintext", false, "talk to the Argo CD server over plain HTTP")
		portFwd     = fs.Bool("port-forward", false, "reach argocd-server through the kubeconfig (like argocd --port-forward)")
		argoConfig  = fs.String("argocd-config", "", "argocd CLI config file (default ~/.config/argocd/config)")
		argoContext = fs.String("argocd-context", "", "context in the argocd CLI config")
		kubeconfig  = fs.String("kubeconfig", "", "kubeconfig path (default: standard loading rules)")
		kubeCtx     = fs.String("context", "", "kubeconfig context")
		showVersion = fs.Bool("version", false, "print version")
		timeout     = fs.Duration("timeout", 60*time.Second, "overall timeout")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, usageText, prog)
		fs.PrintDefaults()
		fmt.Fprintln(stderr, "\nExit codes: 0 synced+healthy, nothing pending; 2 waiting/failed/out-of-sync (explained); 3 unknown (insufficient data); 1 tool error.")
	}
	if err := fs.Parse(args); err != nil {
		if err == pflag.ErrHelp {
			return 0
		}
		return 1
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	pos := fs.Args()
	if len(pos) != 2 || pos[0] != "app" {
		if len(pos) >= 1 && pos[0] == "appset" {
			fmt.Fprintln(stderr, "appset explain is not part of v0.1 (it needs ApplicationSet status semantics); use `app NAME` on a generated Application")
			return 1
		}
		fs.Usage()
		return 1
	}
	if *output != "text" && *output != "json" {
		fmt.Fprintln(stderr, "error: --output must be text or json")
		return 1
	}
	if *argoNS == "" {
		*argoNS = *namespace
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, cancel2 := context.WithTimeout(ctx, *timeout)
	defer cancel2()

	conn, err := collect.ResolveConn(*server, *token, *argoConfig, *argoContext, *insecure, *plainText, env)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	useAPI := false
	switch *mode {
	case "api":
		useAPI = true
	case "kubeconfig":
	case "auto":
		useAPI = *portFwd || conn.Server != ""
	default:
		fmt.Fprintln(stderr, "error: --mode must be auto, api or kubeconfig")
		return 1
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if *kubeconfig != "" {
		rules.ExplicitPath = *kubeconfig
	}
	restCfg, kerr := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: *kubeCtx}).ClientConfig()

	var src collect.Source
	if useAPI {
		if *portFwd {
			if kerr != nil {
				fmt.Fprintln(stderr, "error: --port-forward needs a usable kubeconfig:", kerr)
				return 1
			}
			addr, stop, err := collect.PortForward(ctx, restCfg, *argoNS)
			if err != nil {
				fmt.Fprintln(stderr, "error:", err)
				return 1
			}
			defer stop()
			conn.Server, conn.Insecure, conn.PlainText = addr, true, false
			if conn.Token == "" {
				conn.Token = env("ARGOCD_AUTH_TOKEN")
			}
		}
		if conn.Server == "" {
			fmt.Fprintln(stderr, "error: API mode needs --server (or ARGOCD_SERVER, or the argocd CLI config, or --port-forward)")
			return 1
		}
		if conn.Token == "" {
			fmt.Fprintln(stderr, "error: API mode needs a token (--auth-token, ARGOCD_AUTH_TOKEN, or the argocd CLI config)")
			return 1
		}
		src = collect.NewAPIClient(conn.Server, conn.Token, conn.PlainText, conn.Insecure)
	} else {
		if kerr != nil {
			fmt.Fprintln(stderr, "error: no usable kubeconfig:", kerr)
			return 1
		}
		dyn, err := dynamic.NewForConfig(restCfg)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		src = collect.NewKubeSource(dyn, *argoNS)
		if *mode == "auto" {
			fmt.Fprintln(stderr, "note: no Argo CD API configured; using kubeconfig-only mode (resource health, the resource tree and events are shown as unknown)")
		}
	}

	snap, err := collect.Collect(ctx, src, *namespace, pos[1], time.Now())
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	rep := explain.Explain(snap)
	if *output == "json" {
		err = render.JSON(stdout, rep)
	} else {
		render.Text(stdout, rep)
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return rep.ExitCode
}

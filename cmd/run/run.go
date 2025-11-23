package run

import (
	"archive/tar"
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/homedir"
	"k8s.io/klog/v2"
)

// Global variables for flags
var (
	kubeconfig     string
	namespace      string
	labelSelector  string
	uploadSrc      string
	uploadDest     string
	timeout        time.Duration
	excludePattern string
	excludeRegex   *regexp.Regexp
)

var RunCmd = &cobra.Command{
	Use:   "run [flags] -- [command...]",
	Short: "Run a command or upload files to matching pods",
	Example: `  # Run a command on pods belonging to a JobSet
  krun run --jobset-name=stoelinga -- pip install -r requirements.txt

  # Upload files and run a script
  krun run --label-selector=app=backend --upload-src=./bin --upload-dest=/tmp/bin -- /tmp/bin/start.sh`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Validate inputs
		if len(args) == 0 && uploadSrc == "" {
			klog.Fatal("You must provide either a command (as arguments) or --upload-src (or both)")
		}
		if uploadSrc != "" && uploadDest == "" {
			klog.Fatal("If --upload-src is provided, --upload-dest is required")
		}

		if labelSelector == "" {
			klog.Fatal("You must provide a --label-selector to select target pods")
		}

		// Compile exclude regex if provided
		if excludePattern != "" {
			var err error
			excludeRegex, err = regexp.Compile(excludePattern)
			if err != nil {
				klog.Fatalf("Invalid exclude pattern: %v", err)
			}
		}

		// Setup Context
		rootCtx := cmd.Context()
		var ctx context.Context
		var ctxCancel context.CancelFunc
		if timeout > 0 {
			ctx, ctxCancel = context.WithTimeout(rootCtx, timeout)
		} else {
			ctx, ctxCancel = context.WithCancel(rootCtx)
		}
		defer ctxCancel()

		// Defer error handling for the metrics server
		defer runtime.HandleCrash()

		var config *rest.Config
		var err error
		if kubeconfig == "" {
			if home := homedir.HomeDir(); home != "" {
				kubeconfig = filepath.Join(home, ".kube", "config")
			} else {
				kubeconfig = os.Getenv("KUBECONFIG")
			}
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			klog.Fatalf("can not create client-go configuration: %v", err)
		}

		// creates the clientset
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			klog.Fatalf("can not create client-go client: %v", err)
		}

		klog.V(2).Infof("Listing pods in namespace %q with selector %q", namespace, labelSelector)
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			klog.Fatalf("failed to get pods: %v", err)
		}

		if len(pods.Items) == 0 {
			klog.Infoln("No pods found with selector:", labelSelector)
			return nil
		}

		klog.V(2).Infof("Found %d pods. Starting execution...\n", len(pods.Items))

		cmdArgs := []string{}
		if cmd.ArgsLenAtDash() != -1 {
			cmdArgs = args[cmd.ArgsLenAtDash():]
		}

		return executeOnPods(ctx, config, clientset, pods.Items, cmdArgs)
	},
}

func init() {
	RunCmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	RunCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "default", "Kubernetes namespace")
	RunCmd.Flags().StringVarP(&labelSelector, "label-selector", "l", "", "Label selector for pods (e.g. app=my-app)")
	RunCmd.Flags().StringVar(&uploadSrc, "upload-src", "", "Local path to folder/file to upload")
	RunCmd.Flags().StringVar(&uploadDest, "upload-dest", "", "Remote path (e.g. /tmp/app)")
	RunCmd.Flags().StringVar(&excludePattern, "exclude", "", "Regex pattern to exclude files when uploading")
	RunCmd.Flags().DurationVar(&timeout, "timeout", 0, "Timeout for the execution")
}

func executeOnPods(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, pods []corev1.Pod, commandArgs []string) error {

	klog.V(2).Infof("Found %d pods. Starting execution...\n", len(pods))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// TODO: Limit concurrency with a worker pool if too many pods?
	concurrency := len(pods)
	workerChan := make(chan struct{}, concurrency)
	var printMutex sync.Mutex

	for i, pod := range pods {
		if ctx.Err() != nil {
			klog.Infof("Context done, cancelling remaining %d operations... %v", len(pods)-i, ctx.Err())
			break
		}

		go func(p corev1.Pod) {
			defer func() {
				workerChan <- struct{}{}
			}()
			prefix := fmt.Sprintf("[%s]", p.Name)

			// --- PHASE 1: UPLOAD (TAR STREAMING) ---
			if uploadSrc != "" {
				// We create a pipe: makeTar writes to 'pw', execCmd reads from 'pr'
				pr, pw := io.Pipe()

				// Start Tar Producer
				go func() {
					defer pw.Close() //nolint:errcheck
					if err := makeTar(uploadSrc, pw, excludeRegex); err != nil {
						// Closing with error ensures the execCmd stream fails fast
						pw.CloseWithError(err)
						klog.Errorf("Tar Error: %s %s\n", prefix, err)
						cancel()
					}
				}()

				// 1. Create destination directory
				mkdirCmd := []string{"mkdir", "-p", uploadDest}
				// We must provide at least one stream (stdin, stdout, stderr) for k8s exec.
				err := execCmd(ctx, config, clientset, p, mkdirCmd, nil, io.Discard, nil)
				if err != nil {
					printMutex.Lock()
					_, _ = fmt.Fprintf(os.Stderr, "Mkdir Error: %s %s\n", prefix, err)
					printMutex.Unlock()
					// If mkdir fails, we stop
					return
				}

				// 2. Run 'tar' to consume the stream
				tarCmd := []string{"tar", "-xmf", "-", "-C", uploadDest}

				// Pass 'pr' as Stdin
				err = execCmd(ctx, config, clientset, p, tarCmd, pr, nil, nil)
				if err != nil {
					printMutex.Lock()
					_, _ = fmt.Fprintf(os.Stderr, "Transfer Error: %s %s\n", prefix, err)
					printMutex.Unlock()
					// If upload fails, we probably shouldn't run the command
					return
				}
				printMutex.Lock()
				_, _ = fmt.Fprintf(os.Stderr, "%s Synced %s -> %s\n", prefix, uploadSrc, uploadDest)
				printMutex.Unlock()
			}

			if len(commandArgs) > 0 {
				// Prepare pipes for output
				prOut, pwOut := io.Pipe()
				prErr, pwErr := io.Pipe()

				// Start Log Processors
				go logStream(prOut, &printMutex, prefix, os.Stdout)
				go logStream(prErr, &printMutex, prefix, os.Stderr)

				// Execute
				err := execCmd(ctx, config, clientset, p, commandArgs, nil, pwOut, pwErr)

				_ = pwOut.Close()
				_ = pwErr.Close()

				if err != nil {
					printMutex.Lock()
					_, _ = fmt.Fprintf(os.Stderr, "Command Error: %s %s\n", prefix, err)
					printMutex.Unlock()
				}
			}
		}(pod)
	}

	for range concurrency {
		select {
		case <-ctx.Done():
			klog.Infof("Context done, cancelling remaining operations... %v", ctx.Err())
			os.Exit(1)
		case <-workerChan:
			// One worker finished
		}
	}
	return nil
}

func execCmd(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, pod corev1.Pod, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec")

	option := &corev1.PodExecOptions{
		Command: command,
		Stdin:   stdin != nil,
		Stdout:  stdout != nil,
		Stderr:  stderr != nil,
		TTY:     false,
	}

	req.VersionedParams(option, scheme.ParameterCodec)

	exec, err := remotecommand.NewWebSocketExecutor(config, "GET", req.URL().String())
	if err != nil {
		return err
	}

	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
}

// makeTar walks the source and writes a tarball to the writer
func makeTar(srcPath string, writer io.Writer, excludeRegex *regexp.Regexp) error {
	absSrcPath, err := filepath.Abs(filepath.Clean(srcPath))
	if err != nil {
		return err
	}

	// Check if the source is a directory
	info, err := os.Stat(absSrcPath)
	if err != nil {
		return err
	}

	// If it's a directory, we use the directory itself as the base.
	// This means files inside will have paths relative to the directory,
	// effectively stripping the directory name from the tar archive.
	baseDir := absSrcPath
	if !info.IsDir() {
		// If it's a file, we use its parent as the base, preserving the filename.
		baseDir = filepath.Dir(absSrcPath)
	}

	tw := tar.NewWriter(writer)
	defer tw.Close() //nolint:errcheck

	return filepath.Walk(absSrcPath, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Rebase the path so it's relative to the upload root
		relPath, err := filepath.Rel(baseDir, file)
		if err != nil {
			return err
		}

		// If we are uploading a directory, the walk starts with the directory itself.
		// Its relative path is ".". We skip adding a tar entry for "." to avoid
		// messing with the destination root permissions or creating a "./" folder.
		if relPath == "." {
			return nil
		}

		if excludeRegex != nil && excludeRegex.MatchString(relPath) {
			// If it matches and is a directory, skip the whole tree
			if fi.IsDir() {
				return filepath.SkipDir
			}
			// If it's a file, just skip adding it
			return nil
		}

		// Create header
		header, err := tar.FileInfoHeader(fi, fi.Name())
		if err != nil {
			return err
		}

		header.Name = relPath

		// Ensure binaries are executable (simple heuristic: if we are uploading, preserve local mode)
		// header.Mode is already populated by FileInfoHeader from local file
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !fi.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer f.Close() //nolint:errcheck

		_, err = io.Copy(tw, f)
		return err
	})
}

func logStream(r io.Reader, mu *sync.Mutex, prefix string, out io.Writer) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		text := scanner.Text()
		mu.Lock()
		_, _ = fmt.Fprintf(out, "%s %s\n", prefix, text)
		mu.Unlock()
	}
}

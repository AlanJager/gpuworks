package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/templateprobe"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	var err error
	switch args[0] {
	case "l1":
		err = runL1(args[1:], stdout, stderr)
	case "l2-command":
		err = runL2Command(args[1:], stdout, stderr)
	case "l3":
		err = runL3(args[1:], stdout, stderr)
	case "classify":
		err = runClassify(args[1:], stdout, stderr)
	case "alignment":
		err = runAlignment(args[1:], stdout, stderr)
	case "calibrate":
		err = runCalibration(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown subcommand %q\n", args[0])
		usage(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "template-probe:", err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: template-probe <l1|l2-command|l3|classify|alignment|calibrate> [flags]")
}

func runL1(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("l1", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", "huggingface", "huggingface or modelscope")
	model := flags.String("model", "", "model repository ID")
	revision := flags.String("revision", "", "immutable revision when known")
	endpoint := flags.String("endpoint", "", "optional model repository endpoint")
	tokenEnv := flags.String("token-env", "", "environment variable containing a bearer token")
	imageDigest := flags.String("image-digest", "", "capability snapshot image digest")
	architectures := flags.String("architectures", "", "comma-separated image architecture registry snapshot")
	quantizations := flags.String("quantizations", "", "comma-separated image quantization registry snapshot")
	maxContext := flags.Int64("max-context-length", 0, "image maximum accepted context length")
	indexMetadataOnly := flags.Bool("index-metadata-only", false, "stop after indexed weight size instead of reading every tensor header")
	listWeightFiles := flags.Bool("list-weight-files-fallback", false, "sum repository weight files when standard safetensors metadata is unavailable")
	timeout := flags.Duration("timeout", 30*time.Second, "overall metadata request timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	headers := map[string]string{}
	if *tokenEnv != "" {
		token, ok := os.LookupEnv(*tokenEnv)
		if !ok || strings.TrimSpace(token) == "" {
			return fmt.Errorf("token environment variable %s is empty", *tokenEnv)
		}
		headers["Authorization"] = "Bearer " + strings.TrimSpace(token)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	report, err := (templateprobe.Inspector{
		Fetcher:                      templateprobe.HTTPRangeFetcher{Client: &http.Client{Timeout: *timeout}, Headers: headers},
		SkipTensorHeadersWhenIndexed: *indexMetadataOnly,
		ListWeightFilesFallback:      *listWeightFiles,
	}).Inspect(ctx, templateprobe.L1Input{
		Source: *source, ModelID: *model, Revision: *revision, Endpoint: *endpoint,
		Capabilities: templateprobe.L1Capabilities{
			ImageDigest: *imageDigest, Architectures: splitList(*architectures),
			Quantizations: splitList(*quantizations), MaxContextLength: *maxContext,
		},
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, report)
}

func runL2Command(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("l2-command", flag.ContinueOnError)
	flags.SetOutput(stderr)
	engine := flags.String("engine", "", "vllm or sglang")
	model := flags.String("model", "", "container-visible model path")
	configPath := flags.String("config", "", "config.json path, or - for stdin")
	argvPath := flags.String("argv-json", "", "optional JSON []string base argv path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New("--config is required")
	}
	configBody, err := readInput(*configPath, os.Stdin)
	if err != nil {
		return err
	}
	config, err := templateprobe.ParseModelConfig(configBody)
	if err != nil {
		return err
	}
	var baseArgv []string
	if *argvPath != "" {
		body, err := os.ReadFile(*argvPath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(body, &baseArgv); err != nil {
			return fmt.Errorf("parse base argv: %w", err)
		}
	}
	command, err := templateprobe.BuildL2Command(templateprobe.L2CommandInput{
		Engine: *engine, Model: *model, BaseArgv: baseArgv, Config: config,
	})
	if err != nil {
		return err
	}
	return writeJSON(stdout, command)
}

func runL3(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("l3", flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := flags.String("server", "", "gpuworks management and gateway base URL")
	deploymentPath := flags.String("deployment-json", "", "deployment create-request JSON path")
	casePath := flags.String("case-json", "", "L3 acceptance-case JSON path")
	managementTokenEnv := flags.String("management-token-env", "", "environment variable containing the management JWT")
	apiKeyEnv := flags.String("api-key-env", "", "environment variable containing the data-plane API key")
	timeout := flags.Duration("timeout", 45*time.Minute, "deployment and evaluation timeout")
	cleanupTimeout := flags.Duration("cleanup-timeout", 5*time.Minute, "temporary deployment cleanup timeout")
	pollInterval := flags.Duration("poll-interval", 2*time.Second, "deployment state polling interval")
	keepDeployment := flags.Bool("keep-deployment", false, "retain the created deployment after validation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*server) == "" || strings.TrimSpace(*deploymentPath) == "" || strings.TrimSpace(*casePath) == "" {
		return errors.New("--server, --deployment-json, and --case-json are required")
	}
	deployment, err := os.ReadFile(*deploymentPath)
	if err != nil {
		return fmt.Errorf("read deployment JSON: %w", err)
	}
	caseBody, err := os.ReadFile(*casePath)
	if err != nil {
		return fmt.Errorf("read case JSON: %w", err)
	}
	var testCase templateprobe.L3Case
	if err := json.Unmarshal(caseBody, &testCase); err != nil {
		return fmt.Errorf("parse case JSON: %w", err)
	}
	managementToken, err := readSecretEnv(*managementTokenEnv)
	if err != nil {
		return err
	}
	apiKey, err := readSecretEnv(*apiKeyEnv)
	if err != nil {
		return err
	}
	httpClient := &http.Client{}
	deployer, err := templateprobe.NewHTTPDeploymentClient(*server, managementToken, httpClient)
	if err != nil {
		return err
	}
	deployer.PollInterval = *pollInterval
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, err := (templateprobe.L3Runner{
		Deployer:       deployer,
		Evaluator:      templateprobe.HTTPTaskEvaluator{Client: httpClient, APIKey: apiKey},
		CleanupTimeout: *cleanupTimeout,
	}).Run(ctx, templateprobe.L3Input{
		Deployment: deployment, Case: testCase, KeepDeployment: *keepDeployment,
	})
	if err != nil {
		return err
	}
	if err := writeJSON(stdout, result); err != nil {
		return err
	}
	if result.Status == templateprobe.L3StatusFail || result.Status == templateprobe.L3StatusError {
		return fmt.Errorf("L3 validation status %s", result.Status)
	}
	return nil
}

func runClassify(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("classify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	logPath := flags.String("log", "-", "log path, or - for stdin")
	includeForward := flags.Bool("include-forward", false, "include deterministic forward-kernel signatures")
	if err := flags.Parse(args); err != nil {
		return err
	}
	body, err := readInput(*logPath, os.Stdin)
	if err != nil {
		return err
	}
	return writeJSON(stdout, templateprobe.ClassifyFailure(string(body), templateprobe.ClassifyOptions{IncludeForward: *includeForward}))
}

func runAlignment(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("alignment", flag.ContinueOnError)
	flags.SetOutput(stderr)
	fullStatus := flags.String("full-status", "", "pass or fail")
	fullSignature := flags.String("full-signature", "", "full probe deterministic signature ID")
	truncatedStatus := flags.String("truncated-status", "", "pass or fail")
	truncatedSignature := flags.String("truncated-signature", "", "truncated probe deterministic signature ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	result := templateprobe.CompareOutcomes(
		templateprobe.ProbeOutcome{Status: *fullStatus, SignatureID: *fullSignature},
		templateprobe.ProbeOutcome{Status: *truncatedStatus, SignatureID: *truncatedSignature},
	)
	return writeJSON(stdout, result)
}

func runCalibration(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("calibrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	inputPath := flags.String("input", "-", "hardware calibration record JSON path, or - for stdin")
	outputPath := flags.String("output", "", "optional fixture output path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	body, err := readInput(*inputPath, os.Stdin)
	if err != nil {
		return err
	}
	var input templateprobe.CalibrationRecord
	if err := json.Unmarshal(body, &input); err != nil {
		return fmt.Errorf("parse calibration record: %w", err)
	}
	record, err := templateprobe.NewCalibrationRecord(input)
	if err != nil {
		return err
	}
	if *outputPath != "" {
		if err := templateprobe.WriteCalibrationFixture(*outputPath, record); err != nil {
			return err
		}
	}
	return writeJSON(stdout, record)
}

func readInput(path string, stdin io.Reader) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(io.LimitReader(stdin, 64<<20))
	}
	return os.ReadFile(path)
}

func readSecretEnv(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", nil
	}
	value, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("secret environment variable %s is empty", name)
	}
	return strings.TrimSpace(value), nil
}

func splitList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

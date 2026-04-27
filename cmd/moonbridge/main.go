package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"moonbridge/internal/app"
	"moonbridge/internal/config"
	"moonbridge/internal/logger"
	"moonbridge/internal/server"
)

func main() {
	configPath := flag.String("config", "", "Path to config.yml")
	addr := flag.String("addr", "", "Override server listen address")
	mode := flag.String("mode", "", "Override mode: CaptureAnthropic, CaptureResponse, or Transform")
	printAddr := flag.Bool("print-addr", false, "Print configured listen address and exit")
	printMode := flag.Bool("print-mode", false, "Print configured mode and exit")
	printDefaultModel := flag.Bool("print-default-model", false, "Print configured default model alias and exit")
	printCodexModel := flag.Bool("print-codex-model", false, "Print configured Codex model and exit")
	printClaudeModel := flag.Bool("print-claude-model", false, "Print configured Claude Code model and exit")
	printCodexConfig := flag.String("print-codex-config", "", "Print Codex config.toml for the model alias and exit")
	codexBaseURL := flag.String("codex-base-url", "", "Base URL to write in generated Codex config")
	codexHome := flag.String("codex-home", "", "CODEX_HOME directory; when set, writes models_catalog.json there")
	flag.Parse()

	var cfg config.Config
	var err error
	if *configPath != "" {
		cfg, err = config.LoadFromFile(*configPath)
	} else {
		cfg, err = config.LoadFromFile(config.DefaultConfigPath)
	}
	if err != nil {
		log.Fatal(err)
	}
	if err := logger.Init(logger.Config{Level: logger.Level(cfg.LogLevel), Format: cfg.LogFormat, Output: os.Stderr}); err != nil {
		log.Fatal(err)
	}
	logger.Info("config loaded", "path", *configPath, "mode", cfg.Mode, "addr", cfg.Addr)
	if *mode != "" {
		cfg.Mode = config.Mode(*mode)
		if err := cfg.Validate(); err != nil {
			log.Fatal(err)
		}
	}
	if *addr != "" {
		cfg.OverrideAddr(*addr)
	}
	if *printAddr {
		fmt.Println(cfg.Addr)
		return
	}
	if *printMode {
		fmt.Println(cfg.Mode)
		return
	}
	if *printDefaultModel {
		fmt.Println(cfg.DefaultModelAlias())
		return
	}
	if *printCodexModel {
		fmt.Println(cfg.CodexModel())
		return
	}
	if *printClaudeModel {
		fmt.Println(cfg.AnthropicProxy.Model)
		return
	}
	if *printCodexConfig != "" {
		printCodexConfigToml(*printCodexConfig, *codexBaseURL, *codexHome, cfg)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	if err := app.RunServer(ctx, cfg, os.Stderr); err != nil {
		log.Fatal(err)
	}
}

func printCodexConfigToml(modelAlias string, baseURL string, codexHome string, cfg config.Config) {
	route := cfg.RouteFor(modelAlias)
	fmt.Printf("model = %q\n", modelAlias)
	fmt.Println(`model_provider = "moonbridge"`)
	if route.ContextWindow > 0 {
		fmt.Printf("model_context_window = %d\n", route.ContextWindow)
	}
	if route.MaxOutputTokens > 0 {
		fmt.Printf("model_max_output_tokens = %d\n", route.MaxOutputTokens)
	}

	// Write models catalog JSON so Codex uses our metadata instead of bundled presets.
	if codexHome != "" {
		catalogPath := filepath.Join(codexHome, "models_catalog.json")
		if err := writeModelsCatalog(catalogPath, cfg); err != nil {
			log.Fatalf("failed to write models catalog: %v", err)
		}
		fmt.Printf("model_catalog_json = %q\n", catalogPath)
	}

	fmt.Println()
	fmt.Println("[model_providers.moonbridge]")
	fmt.Println(`name = "Moon Bridge"`)
	fmt.Printf("base_url = %q\n", valueOrDefault(baseURL, "http://"+config.DefaultAddr+"/v1"))
	fmt.Println(`env_key = "MOONBRIDGE_CLIENT_API_KEY"`)
	fmt.Println(`wire_api = "responses"`)
	fmt.Println()
	fmt.Println("[mcp_servers.deepwiki]")
	fmt.Println(`url = "https://mcp.deepwiki.com/mcp"`)
	fmt.Println("startup_timeout_sec = 3600")
	fmt.Println("tool_timeout_sec = 3600")
}

func valueOrDefault(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// writeModelsCatalog generates a Codex-compatible models_catalog.json from all routes.
func writeModelsCatalog(path string, cfg config.Config) error {
	var models []server.ModelInfo
	for alias, route := range cfg.Routes {
		ownedBy := "system"
		if route.Provider != "" {
			ownedBy = route.Provider
		}
		models = append(models, server.BuildModelInfoFromRoute(alias, ownedBy, route))
	}
	catalog := struct {
		Models []server.ModelInfo `json:"models"`
	}{Models: models}
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

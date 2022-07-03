package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/goccy/go-json"
	"github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/option"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func init() {
	logrus.StandardLogger().SetLevel(logrus.TraceLevel)
	logrus.StandardLogger().Formatter.(*logrus.TextFormatter).ForceColors = true
}

var (
	configPath   string
	workingDir   string
	formatConfig bool
)

func main() {
	command := &cobra.Command{
		Use: "sing-box",
		Run: run,
	}
	command.Flags().StringVarP(&configPath, "config", "c", "config.json", "set configuration file path")
	command.Flags().StringVarP(&workingDir, "directory", "D", "", "set working directory")
	command.Flags().BoolVarP(&formatConfig, "format", "f", false, "print formatted configuration file")
	if err := command.Execute(); err != nil {
		logrus.Fatal(err)
	}
}

func run(cmd *cobra.Command, args []string) {
	if workingDir != "" {
		if err := os.Chdir(workingDir); err != nil {
			logrus.Fatal(err)
		}
	}

	configContent, err := os.ReadFile(configPath)
	if err != nil {
		logrus.Fatal("read config: ", err)
	}
	var options option.Options
	err = json.Unmarshal(configContent, &options)
	if err != nil {
		logrus.Fatal("decode config: ", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	service, err := box.NewService(ctx, options)
	if err != nil {
		logrus.Fatal("create service: ", err)
	}

	if formatConfig {
		cancel()
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(options)
		if err != nil {
			logrus.Fatal("encode config: ", err)
		}
		return
	}

	err = service.Start()
	if err != nil {
		logrus.Fatal("start service: ", err)
	}
	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM)
	<-osSignals
	cancel()
	service.Close()
}
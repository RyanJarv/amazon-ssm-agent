package main

import (
	"os"
	"time"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/context"
	"github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/framework/processor/executer/outofproc/messaging"
	"github.com/aws/amazon-ssm-agent/agent/framework/processor/executer/outofproc/proc"
	"github.com/aws/amazon-ssm-agent/agent/framework/processor/executer/plugin"
	"github.com/aws/amazon-ssm-agent/agent/framework/runpluginutil"
	"github.com/aws/amazon-ssm-agent/agent/log"
	"github.com/aws/amazon-ssm-agent/agent/log/ssmlog"
	"github.com/aws/amazon-ssm-agent/agent/task"
	"github.com/aws/amazon-ssm-agent/agent/version"
	"github.com/aws/amazon-ssm-agent/common/filewatcherbasedipc"
	"github.com/aws/amazon-ssm-agent/common/identity"
)

const (
	defaultCommandTimeoutMax = 172800 * time.Second
	defaultWorkerContextName = "[ssm-document-worker]"
)

var pluginRunner = func(
	context context.T,
	docState contracts.DocumentState,
	resChan chan contracts.PluginResult,
	cancelFlag task.CancelFlag,
) {
	runpluginutil.RunPlugins(context, docState.InstancePluginsInformation, docState.IOConfig, runpluginutil.SSMPluginRegistry, resChan, cancelFlag)
	//make sure to signal the client that job complete
	close(resChan)
}

//TODO revisit this, is plugin entitled to use appconfig?
//TODO add log level to args
//rule of thumb is, do not trigger extra file operation or other intricate dependencies during this setup, make it light weight
func initialize(args []string) (context.T, string, error) {
	// intialize a light weight logger, use the default seelog config logger
	logger := ssmlog.SSMLogger(false)
	// initialize appconfig, default config is provided in case of error
	config, _ := appconfig.Config(true)
	logger.Infof("ssm-document-worker - %v", version.String())
	logger.Infof("parsing args: %v", args)
	channelName, err := proc.ParseArgv(args)
	//cache the instanceID here in order to avoid throttle by metadata endpoint.

	selector := identity.NewRuntimeConfigIdentitySelector(logger)
	agentIdentity, err := identity.NewAgentIdentity(logger, &config, selector)
	if err != nil {
		return nil, "", err
	}

	instanceID, err := agentIdentity.InstanceID()
	if err != nil {
		return nil, "", err
	}

	logger.Infof("using channelName %v, instanceID: %v", channelName, instanceID)

	//use process as context name
	return context.Default(logger, config, agentIdentity).With(defaultWorkerContextName).With("[" + channelName + "]"), channelName, err
}

func main() {
	var err error
	var logger log.T
	args := os.Args
	ctx, channelName, err := initialize(args)
	logger = ctx.Log()
	if err != nil {
		logger.Errorf("document worker failed to initialize, exit")
		logger.Close()
		return
	}
	logger.Infof("document: %v worker started", channelName)
	//create channel from the given handle identifier by master
	ipc, err, _ := filewatcherbasedipc.CreateFileWatcherChannel(logger, ctx.Identity(), filewatcherbasedipc.ModeWorker, channelName, false)
	if err != nil {
		logger.Errorf("failed to create channel: %v", err)
		logger.Close()
		return
	}
	//initialize PluginRegistry
	runpluginutil.SSMPluginRegistry = plugin.RegisteredWorkerPlugins(ctx)

	//TODO add command timeout
	stopTimer := make(chan bool)
	pipeline := messaging.NewWorkerBackend(ctx, pluginRunner)
	//TODO wait for sigterm or send fail message to the channel?
	if err = messaging.Messaging(ctx.Log(), ipc, pipeline, stopTimer); err != nil {
		logger.Errorf("messaging worker encountered error: %v", err)
		//If ipc messaging broke, there's nothing worker process can do, exit immediately
		logger.Close()
		return
	}
	logger.Info("document worker closed")
	//ensure logs are flushed
	logger.Close()
	//TODO figure out s3 aync problem
	//TODO figure out why defer main doesnt work on windows
	if err != nil {
		os.Exit(1)
	}
}

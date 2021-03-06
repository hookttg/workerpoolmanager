package taskmanager

import (
	"errors"
	"log"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/quipo/goprofiler/profiler"
	jqutils "github.com/quipo/workerpoolmanager/utils"
)

// KeepAliveConf contains the configuration for Keep-Alive handler and ZeroMQ channel
type KeepAliveConf struct {
	InboundPort  int    `json:"inbound_port,omitempty"`
	InternalPort int    `json:"internal_port,omitempty"`
	Host         string `json:"host,omitempty"`
	StallTimeout int64  `json:"stall_timeout,omitempty"`
	GracePeriod  int64  `json:"grace_period,omitempty"`
}

// TaskManagerConf contains the configuration for the Task Manager
type TaskManagerConf struct {
	Path         string                 `json:"path,omitempty"`
	FileSuffix   string                 `json:"filesuffix,omitempty"`
	Port         int                    `json:"port,omitempty"`
	Autotasks    map[string]TaskManager `json:"autotasks,omitempty"`
	Keepalives   KeepAliveConf          `json:"keepalives,omitempty"`
	ForceTimeout int64                  `json:"force_timeout"`
	Profiler     profiler.Config        `json:"profiler"`
}

// Runner is a container for Task Managers
type Runner struct {
	Conf TaskManagerConf
	// private struct members
	taskManagers  map[string]TaskManager
	taskCommands  map[string]chan Command
	inputCommands chan Command
	logger        *log.Logger
}

// NewRunner Returns an instance of a Task Manager Runner
func NewRunner(taskMgrConf TaskManagerConf) (Runner, error) {
	taskRunner := Runner{Conf: taskMgrConf}
	taskRunner.taskManagers = make(map[string]TaskManager, 0)
	taskRunner.inputCommands = make(chan Command, 10)
	taskRunner.logger = log.New(os.Stdout, "[Runner] ", log.Ldate|log.Ltime)

	// load all available executable files from the path, having the wanted suffix
	taskfiles := jqutils.Filter(jqutils.ListFiles(taskMgrConf.Path), func(v string) bool {
		return strings.HasSuffix(v, taskMgrConf.FileSuffix)
	})
	if len(taskfiles) < 1 {
		taskRunner.logger.Println("NewRunner() - no tasks found at path", taskMgrConf.Path)
		os.Exit(0)
	}

	for filename, taskname := range jqutils.KMap(taskfiles, func(v string) string {
		return strings.TrimSuffix(v, taskMgrConf.FileSuffix)
	}) {
		taskMgr := NewTaskManager(taskname, taskMgrConf.Keepalives, taskRunner.inputCommands)
		taskMgr.Cardinality = 1
		taskMgr.AutoStart = false
		taskMgr.Active = false
		taskMgr.Cmd = path.Join(taskMgrConf.Path, filename)
		taskRunner.taskManagers[taskname] = taskMgr
	}
	// override auto-start tasks
	for taskname, autotask := range taskMgrConf.Autotasks {
		task, ok := taskRunner.taskManagers[taskname]
		if !ok {
			return taskRunner, errors.New("Cannot find task " + taskname)
		}
		task.AutoStart = true
		task.CopyFrom(autotask, taskMgrConf.Path)
		taskRunner.taskManagers[taskname] = task
	}

	return taskRunner, nil
}

// Run a task, and keep its workers at the desired cardinality
func (taskRunner *Runner) Run() {
	//runtime.GOMAXPROCS(runtime.NumCPU())
	runtime.GOMAXPROCS(2*len(taskRunner.taskManagers) + 2)

	taskRunner.logger.Println("Run()")

	// init HTTP, signal and keep-alive handlers
	taskRunner.taskCommands = make(map[string]chan Command)

	httpHandler := HTTPHandler{
		CommandChannel: taskRunner.inputCommands,
		Host:           "localhost",
		Port:           taskRunner.Conf.Port,
		Logger:         log.New(os.Stdout, "[HttpHandler] ", log.Ldate|log.Ltime),
	}
	signalHandler := SignalHandler{
		CommandChannel: taskRunner.inputCommands,
		Logger:         log.New(os.Stdout, "[SignalHandler] ", log.Ldate|log.Ltime),
		ForceTimeout:   taskRunner.Conf.ForceTimeout,
	}

	// start a ZeroMQ PUB-SUB proxy (many-to-many device)
	zmqConf := taskRunner.Conf.Keepalives
	go jqutils.ZmqPubSubProxy(
		zmqConf.Host,
		zmqConf.InboundPort,
		zmqConf.InternalPort,
		log.New(os.Stdout, "[ZeromqProxy] ", log.Ldate|log.Ltime))

	go signalHandler.Run()
	go httpHandler.Run()

	// start AutoStart tasks
	for name, task := range taskRunner.taskManagers {
		taskRunner.taskCommands[name] = make(chan Command, 10)

		if task.AutoStart {
			task.Active = true
			taskRunner.taskManagers[name] = task
			// auto-start
			go func(t TaskManager, ch chan Command) {
				replyChan := make(chan CommandReply, 1)
				t.Start(ch, Command{Type: "start", ReplyChannel: replyChan})
				t.logger.Println(<-replyChan)
			}(task, taskRunner.taskCommands[name])
		}
	}

	// loop forever, checking status/update commands
	var cmd Command
	for {
		select {
		case cmd = <-taskRunner.inputCommands:
			taskRunner.logger.Println("Received command:", cmd)
			taskRunner.processCommand(cmd)
		case <-time.After(10 * time.Second):
			taskRunner.logger.Println("Checking update commands")
		}
	}

	//taskRunner.logger.Println("terminating")
	//os.Exit(0)
}

// ListTasks - List available tasks
func (taskRunner *Runner) ListTasks() []string {
	var tasks []string
	for name := range taskRunner.taskManagers {
		tasks = append(tasks, name)
	}
	sort.Strings(tasks)
	return tasks
}

// Deal with a single command at a time (it might need to be forwarded
// to the command channel of a specific task manager)
func (taskRunner *Runner) processCommand(cmd Command) bool {
	if !taskRunner.validateCommand(cmd) {
		return false
	}

	//FIXME setting active flag on a goroutine

	// process
	switch cmd.Type {
	case "list":
		for _, taskname := range taskRunner.ListTasks() {
			cmd.ReplyChannel <- CommandReply{Reply: taskname, Error: nil}
		}
		close(cmd.ReplyChannel)
		return true
	case "start":
		task, _ := taskRunner.taskManagers[cmd.TaskName]
		if !task.Active {
			// fresh command channel to avoid getting messages from a previous task instance
			taskRunner.taskCommands[cmd.TaskName] = make(chan Command, 10)
			task.Active = true
			taskRunner.taskManagers[cmd.TaskName] = task
			go func(ch chan Command) {
				replyChan := make(chan CommandReply, 1)
				task.Start(ch, Command{Type: "start", ReplyChannel: replyChan})
				task.logger.Println(<-replyChan)
			}(taskRunner.taskCommands[cmd.TaskName])
			cmd.Success("Started task" + cmd.TaskName)
			return true
		}
		return cmd.Success("[Runner] Already running " + cmd.TaskName)
	case "status":
		if "" == cmd.TaskName {
			taskRunner.logger.Println(taskRunner.getChannelsOfActiveTasks())
			return cmd.Broadcast(taskRunner.getChannelsOfActiveTasks())
		}
	case "stopped":
		task := taskRunner.taskManagers[cmd.TaskName]
		task.Active = false
		taskRunner.taskManagers[cmd.TaskName] = task
		return cmd.Success("[Runner] Received 'stopped' notification from task")
	case "stop":
		// mark active tasks as inactive first, then forward the STOP cmd
		if "" == cmd.TaskName {
			taskChannels := taskRunner.getChannelsOfActiveTasks()
			for name, task := range taskRunner.taskManagers {
				if task.Active {
					task.Active = false
					taskRunner.taskManagers[name] = task
				}
			}
			return cmd.Broadcast(taskChannels)
		}
		task := taskRunner.taskManagers[cmd.TaskName]
		task.Active = false
		taskRunner.taskManagers[cmd.TaskName] = task
	case "set":
	case "listworkers":
		// forward command
	}

	return cmd.Forward(taskRunner.taskCommands[cmd.TaskName])
}

// Validate a command before executing/forwarding it
func (taskRunner *Runner) validateCommand(cmd Command) bool {
	// step 1: verify it's a known command
	switch cmd.Type {
	case "list", "listworkers", "set", "start", "status", "stop", "stopped", "kill":
		// ok
	default:
		taskRunner.logger.Println("ERROR: unknown command")
		return cmd.Fail("ERROR: unknown command")
	}

	// step 2: if a task is specified in the command, verify it's a known task
	if "" != cmd.TaskName {
		_, ok := taskRunner.taskManagers[cmd.TaskName]
		if !ok {
			taskRunner.logger.Println("ERROR: trying to start/update unknown task " + cmd.TaskName)
			return cmd.Fail("ERROR: trying to start/update unknown task " + cmd.TaskName)
		}
	}

	// step 3: some commands require a task name
	switch cmd.Type {
	case "listworkers", "set", "start", "stopped":
		if "" == cmd.TaskName {
			taskRunner.logger.Println("ERROR: start/update command on task with no name")
			return cmd.Fail("ERROR: start/update command on task with no name")
		}
	}

	// step 4: verify the command is for an active task
	switch cmd.Type {
	case "listworkers", "set", "status", "stop", "kill":
		if "" != cmd.TaskName {
			task, _ := taskRunner.taskManagers[cmd.TaskName]
			if !task.Active {
				taskRunner.logger.Println("Task " + cmd.TaskName + " not running")
				return cmd.Fail("Task " + cmd.TaskName + " not running")
			}
		}
	}

	return true
}

// Filter command channels by active task status
func (taskRunner *Runner) getChannelsOfActiveTasks() map[int]chan Command {
	channels := make(map[int]chan Command)
	i := 0
	for name, task := range taskRunner.taskManagers {
		if task.Active {
			channels[i] = taskRunner.taskCommands[name]
			i++
		}
	}
	return channels
}

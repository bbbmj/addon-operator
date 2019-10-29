package kube_event

import (
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/flant/shell-operator/pkg/hook"
	"github.com/flant/shell-operator/pkg/hook/kube_event"
	"github.com/flant/shell-operator/pkg/kube_events_manager"

	"github.com/flant/addon-operator/pkg/module_manager"
	"github.com/flant/addon-operator/pkg/task"
	"github.com/flant/addon-operator/pkg/utils"
)


type KubernetesHooksController interface {
	WithModuleManager(moduleManager module_manager.ModuleManager)
	WithKubeEventsManager(kube_events_manager.KubeEventsManager)
	EnableGlobalHooks(logLabels map[string]string) error
	EnableModuleHooks(moduleName string, logLabels map[string]string) error
	DisableModuleHooks(moduleName string, logLabels map[string]string) error
	HandleEvent(kubeEvent kube_events_manager.KubeEvent, logLabels map[string]string) ([]task.Task, error)
}

type kubernetesHooksController struct {
	// storages for registered module hooks and global hooks
	GlobalHooks    map[string]*kube_event.KubeEventHook
	ModuleHooks    map[string]*kube_event.KubeEventHook
	EnabledModules []string

	// dependencies
	moduleManager  module_manager.ModuleManager
	kubeEventsManager kube_events_manager.KubeEventsManager
}

// kubernetesHooksController should implement KubernetesHooksController
var _ KubernetesHooksController = &kubernetesHooksController{}

// NewKubernetesHooksController returns new instance of kubernetesHooksController
func NewKubernetesHooksController() *kubernetesHooksController {
	obj := &kubernetesHooksController{}
	obj.GlobalHooks = make(map[string]*kube_event.KubeEventHook)
	obj.ModuleHooks = make(map[string]*kube_event.KubeEventHook)
	obj.EnabledModules = make([]string, 0)
	return obj
}

func (c *kubernetesHooksController) WithModuleManager(mm module_manager.ModuleManager) {
	c.moduleManager = mm
}

func (c *kubernetesHooksController) WithKubeEventsManager(kem kube_events_manager.KubeEventsManager) {
	c.kubeEventsManager = kem
}

// EnableGlobalHooks starts kube events informers for all global hooks
func (c *kubernetesHooksController) EnableGlobalHooks(logLabels map[string]string) error {
	globalHooks := c.moduleManager.GetGlobalHooksInOrder(module_manager.KubeEvents)

	for _, globalHookName := range globalHooks {
		globalHook, _ := c.moduleManager.GetGlobalHook(globalHookName)

		for _, config := range globalHook.Config.OnKubernetesEvents {
			logEntry := log.WithFields(utils.LabelsToLogFields(logLabels)).
				WithField("hook", globalHook.Name).
				WithField("hook.type", "global")
			err := c.kubeEventsManager.AddMonitor("", config.Monitor, logEntry)
			if err != nil {
				return fmt.Errorf("run kube monitor for hook %s: %s", globalHook.Name, err)
			}
			c.GlobalHooks[config.Monitor.Metadata.ConfigId] = &kube_event.KubeEventHook{
				HookName:     globalHook.Name,
				ConfigName:   config.ConfigName,
				AllowFailure: config.AllowFailure,
			}
		}
	}

	// Start created informers
	c.kubeEventsManager.Start()

	return nil
}

// EnableModuleHooks starts kube events informers for all module hooks
func (c *kubernetesHooksController) EnableModuleHooks(moduleName string, logLabels map[string]string) error {
	for _, enabledModuleName := range c.EnabledModules {
		if enabledModuleName == moduleName {
			// already enabled
			return nil
		}
	}

	moduleHooks, err := c.moduleManager.GetModuleHooksInOrder(moduleName, module_manager.KubeEvents)
	if err != nil {
		return err
	}

	for _, moduleHookName := range moduleHooks {
		moduleHook, _ := c.moduleManager.GetModuleHook(moduleHookName)

		for _, config := range moduleHook.Config.OnKubernetesEvents {
			logEntry := log.WithFields(utils.LabelsToLogFields(logLabels)).
				WithField("hook", moduleHook.Name).
				WithField("hook.type", "module").
				WithField("module", moduleHook.Module.Name)
			err := c.kubeEventsManager.AddMonitor("", config.Monitor, logEntry)
			if err != nil {
				return fmt.Errorf("run kube monitor for hook %s: %s", moduleHook.Name, err)
			}
			c.ModuleHooks[config.Monitor.Metadata.ConfigId] = &kube_event.KubeEventHook{
				HookName:     moduleHook.Name,
				ConfigName:   config.ConfigName,
				AllowFailure: config.AllowFailure,
			}
		}
	}

	c.EnabledModules = append(c.EnabledModules, moduleName)

	// Start created informers
	c.kubeEventsManager.Start()
	return nil
}

// DisableModuleHooks stops all monitors for all hooks in module
func (c *kubernetesHooksController) DisableModuleHooks(moduleName string, logLabels map[string]string) error {
	// TODO remove EnabledModules index. ConfigId is now in  moduleHook.Config.OnKubernetesEvents[].Monitor.Metadata.ConfigId
	// loop through module hooks and check if configId is in c.ModuleHooks, stop monitor and delete a map item.
	moduleEnabledInd := -1
	for i, enabledModuleName := range c.EnabledModules {
		if enabledModuleName == moduleName {
			moduleEnabledInd = i
			break
		}
	}
	if moduleEnabledInd < 0 {
		return nil
	}
	// remove name from enabled modules index
	c.EnabledModules = append(c.EnabledModules[:moduleEnabledInd], c.EnabledModules[moduleEnabledInd+1:]...)

	disabledModuleHooks, err := c.moduleManager.GetModuleHooksInOrder(moduleName, module_manager.KubeEvents)
	if err != nil {
		return err
	}

	for configId, desc := range c.ModuleHooks {
		for _, disabledModuleHookName := range disabledModuleHooks {
			if desc.HookName == disabledModuleHookName {
				err := c.kubeEventsManager.StopMonitor(configId)
				if err != nil {
					return err
				}

				delete(c.ModuleHooks, configId)

				break
			}
		}
	}

	return nil
}

// HandleEvent creates a task from kube event
func (c *kubernetesHooksController) HandleEvent(kubeEvent kube_events_manager.KubeEvent, logLabels map[string]string) ([]task.Task, error) {
	res := make([]task.Task, 0)

	globalEventHook, hasGlobalHook := c.GlobalHooks[kubeEvent.ConfigId]
	moduleEventHook, hasModuleHook := c.ModuleHooks[kubeEvent.ConfigId]
	if !hasGlobalHook && !hasModuleHook {
		return nil, fmt.Errorf("Possible a bug: kubernetes event '%s/%s/%s %s' is received, but no hook is found", kubeEvent.Namespace, kubeEvent.Kind, kubeEvent.Name, kubeEvent.Event)
	}


	hookLabels := utils.MergeLabels(logLabels)

	var taskType task.TaskType
	var kubeHook *kube_event.KubeEventHook
	var configVersion string
	if hasGlobalHook {
		taskType = task.GlobalHookRun
		kubeHook = globalEventHook
		globalHook, _ := c.moduleManager.GetGlobalHook(globalEventHook.HookName)
		configVersion = globalHook.Config.Version
		hookLabels["hook"] = globalEventHook.HookName
		hookLabels["hook.type"] = "global"
	} else {
		taskType = task.ModuleHookRun
		kubeHook = moduleEventHook
		moduleHook, _ := c.moduleManager.GetModuleHook(moduleEventHook.HookName)
		configVersion = moduleHook.Config.Version
		hookLabels["hook"] = moduleEventHook.HookName
		hookLabels["hook.type"] = "module"
	}

	switch kubeEvent.Type {
	case "Synchronization":
		// Ignore Synchronization for v0
		if configVersion == "v0" {
			break
		}
		// Send all objects
		objList := make([]interface{}, 0)
		for _, obj := range kubeEvent.Objects {
			objList = append(objList, interface{}(obj))
		}
		bindingContext := make([]module_manager.BindingContext, 0)
		bindingContext = append(bindingContext, module_manager.BindingContext{
			BindingContext: hook.BindingContext{
				Binding: kubeHook.ConfigName,
				Type:    kubeEvent.Type,
				Objects: objList,
			},
		})

		newTask := task.NewTask(taskType, kubeHook.HookName).
			WithBinding(module_manager.KubeEvents).
			WithBindingContext(bindingContext).
			WithAllowFailure(kubeHook.AllowFailure).
			WithLogLabels(hookLabels)

		res = append(res, newTask)
	default:
		bindingContext := make([]module_manager.BindingContext, 0)
		for _, kEvent := range kubeEvent.WatchEvents {
			bindingContext = append(bindingContext, module_manager.BindingContext{
				BindingContext:	hook.BindingContext{
				Binding:    kubeHook.ConfigName,
				Type:       "Event",
				WatchEvent: kEvent,

				Namespace: kubeEvent.Namespace,
				Kind:      kubeEvent.Kind,
				Name:      kubeEvent.Name,

				Object:       kubeEvent.Object,
				FilterResult: kubeEvent.FilterResult,
			},
			})
		}

		newTask := task.NewTask(taskType, kubeHook.HookName).
			WithBinding(module_manager.KubeEvents).
			WithBindingContext(bindingContext).
			WithAllowFailure(kubeHook.AllowFailure).
			WithLogLabels(hookLabels)

		res = append(res, newTask)
	}

	return res, nil
}

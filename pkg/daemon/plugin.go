package daemon

import (
	"github.com/golang/glog"
	"github.com/k8snetworkplumbingwg/linuxptp-daemon/addons"
	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/plugin"
	ptpv1 "github.com/k8snetworkplumbingwg/ptp-operator/api/v1"
)

func registerPlugins(plugins []string) plugin.PluginManager {
	glog.Infof("Begin plugin registration...")
	validatePluginMapping()
	manager := plugin.PluginManager{Plugins: make(map[string]*plugin.Plugin),
		Data: make(map[string]*interface{}),
	}
	for _, name := range plugins {
		currentPlugin, currentData := registerPlugin(name)
		if currentPlugin != nil {
			manager.Plugins[name] = currentPlugin
			manager.Data[name] = currentData
		}
	}
	return manager
}

// validatePluginMapping checks that the plugin mapping matches the operator API
func validatePluginMapping() {
	// Check that all available plugins from operator API are in the mapping
	for _, pluginName := range ptpv1.AvailablePlugins {
		if _, exists := mapping.PluginMapping[pluginName]; !exists {
			glog.Warningf("Plugin '%s' is listed in operator API but not implemented in daemon", pluginName)
		}
	}

	// Check that all plugins in mapping are in the operator API
	for pluginName := range mapping.PluginMapping {
		found := false
		for _, available := range ptpv1.AvailablePlugins {
			if pluginName == available {
				found = true
				break
			}
		}
		if !found {
			glog.Warningf("Plugin '%s' is implemented in daemon but not listed in operator API", pluginName)
		}
	}
}

func registerPlugin(name string) (*plugin.Plugin, *interface{}) {
	glog.Infof("Trying to register plugin: " + name)
	for mName, mConstructor := range mapping.PluginMapping {
		if mName == name {
			return mConstructor(name)
		}
	}
	glog.Errorf("Plugin not found: %s. Available plugins: %v", name, ptpv1.AvailablePlugins)
	return nil, nil
}

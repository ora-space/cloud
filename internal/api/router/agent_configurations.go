package router

// AgentConfigurationPath addresses the user's one plugin configuration in a selected space's
// release context. The namespace and identifier together are the canonical plugin identity.
const AgentConfigurationPath = "/api/v1/me/agent-configurations/:spaceId/:pluginNamespace/:pluginIdentifier"

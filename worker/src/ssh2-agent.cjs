function UnsupportedAgentContext() {
  throw new Error("SSH agent authentication is unavailable");
}

module.exports = {
  AgentContext: UnsupportedAgentContext,
  createAgent: () => undefined,
  isAgent: () => false,
};

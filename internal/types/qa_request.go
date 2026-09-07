package types

// QARequest consolidates all parameters for KnowledgeQA and AgentQA service calls,
// replacing the previous 14-parameter method signatures.
// EventBus is passed separately to avoid circular dependency with the event package.
type QARequest struct {
	Session             *Session           // The conversation session
	Query               string             // User query text
	AssistantMessageID  string             // Pre-created assistant message ID
	SummaryModelID      string             // Optional model override; empty = use agent/KB default
	CustomAgent         *CustomAgent       // Optional custom agent for config override
	SharedAgentReadOnly bool               // True only when access came from an agent share; source-workspace writes are forbidden
	KnowledgeBaseIDs    []string           // Knowledge base IDs to search (from request + @mentions)
	KnowledgeIDs        []string           // Specific knowledge (file) IDs to search
	TagScopes           []TagScope         // Tag-constrained KB scopes from @mentions
	MCPServiceIDs       []string           // Per-request MCP service IDs from @mentions
	SkillNames          []string           // Per-request skill names from @mentions
	ImageURLs           []string           // Image URLs for multimodal input
	ImageDescription    string             // VLM-generated image description (fallback for non-vision models)
	UserMessageID       string             // Created user message ID
	WebSearchEnabled    bool               // Whether web search is enabled for this request
	QuotedContext       string             // Quoted message content from IM quote-reply (appended at LLM prompt stage, not used for retrieval)
	Attachments         MessageAttachments // File attachments (processed and ready for prompt injection)
	// Metadata is caller-supplied structured context (JSON) attached to this
	// request. It is injected verbatim into the agent context so the model can
	// consume custom data (page, search fields, condition rules, ...) that is
	// not backed by a knowledge base. It is not persisted on the user message.
	Metadata JSON `json:"metadata,omitempty"`
}

package modelmeta

// officialCNYPrices contains vendor-published list prices in CNY per million
// tokens. It intentionally contains only ids whose model identity is clear.
// Opaque router ids stay absent and therefore keep rendering as unknown.
//
// For tiered prices the model list shows the standard lowest tier. That keeps
// the existing single-value Price contract while avoiding a fabricated
// blended rate.
var officialCNYPrices = map[string]Price{
	// Zhipu / BigModel, CNY list prices.
	"glm-5.3":        {8, 28, 2, true, true},
	"glm-5.3-flash":  {0.8, 2.8, 0.23, true, true},
	"glm-5.3-flashx": {2, 7, 0.57, true, true},
	"glm-5.2":        {8, 28, 2, true, true},
	"glm-5.1":        {6, 24, 1.3, true, true},
	"glm-5-turbo":    {5, 22, 1.2, true, true},
	"glm-5":          {4, 18, 1, true, true},
	"glm-5v-turbo":   {5, 22, 1.2, true, true},
	"glm-4.7":        {2, 8, 0.4, true, true},
	"glm-4.6v":       {1, 3, 0.2, true, true},
	"glm-4.5-air":    {0.8, 2, 0.16, true, true},

	// Alibaba Model Studio CN, CNY list prices.
	"qwen3.8-max":        {12, 36, 1.5, true, true},
	"qwen3.7-plus":       {2, 8, 0.4, true, true},
	"qwen3.8-flash":      {0.8, 2.7, 0.1, true, true},
	"qwen3.8-omni-flash": {0.8136, 2.7504, 0.1008, true, true},

	// Volcengine Ark / Doubao, CNY list prices.
	"doubao-seed-2.1-pro":          {6, 30, 1.2, true, true},
	"doubao-seed-2-1-pro":          {6, 30, 1.2, true, true},
	"doubao-seed-2.1-turbo":        {3, 15, 0.6, true, true},
	"doubao-seed-2-1-turbo":        {3, 15, 0.6, true, true},
	"doubao-seed-2.1-lite":         {0.8, 2.7, 0.16, true, true},
	"doubao-seed-2-1-lite":         {0.8, 2.7, 0.16, true, true},
	"doubao-seed-2.0-lite":         {0.6, 3.6, 0.12, true, true},
	"doubao-seed-2-0-lite":         {0.6, 3.6, 0.12, true, true},
	"doubao-seed-2.1-pro-260628":   {6, 30, 1.2, true, true},
	"doubao-seed-2-1-pro-260628":   {6, 30, 1.2, true, true},
	"doubao-seed-2.1-turbo-260628": {3, 15, 0.6, true, true},
	"doubao-seed-2-1-turbo-260628": {3, 15, 0.6, true, true},
	"doubao-seed-evolving":         {6, 30, 1.2, true, true},

	// Tencent Hunyuan, CNY list prices.
	"hy4-preview": {6, 18, 0.3, true, true},
	"hy3":         {1, 4, 0.25, true, true},

	// Xiaomi MiMo, CNY list prices.
	"mimo-v2.6-pro":            {3, 6, 0.025, true, true},
	"mimo-v2.6-flash":          {1, 2, 0.02, true, true},
	"mimo-v2.6-pro-ultraspeed": {30, 60, 0.25, true, true},

	// Kimi official USD list prices converted at the project rate (7.20).
	"kimi-k3":                  {21.6, 108, 2.16, true, true},
	"kimi-k2.7-code":           {6.84, 28.8, 1.368, true, true},
	"kimi-k2.7-code-highspeed": {13.68, 57.6, 2.736, true, true},
	"kimi-k2.6":                {6.84, 28.8, 1.152, true, true},

	// MiniMax official USD list prices converted at the project rate (7.20).
	"minimax-m3":             {2.16, 8.64, 0.432, true, true},
	"minimax-m2.7":           {2.16, 8.64, 0.432, true, true},
	"minimax-m2.7-highspeed": {4.32, 17.28, 0.432, true, true},

	// Anthropic official USD list prices converted at the project rate (7.20).
	"claude-opus-5.5":   {28.8, 144, 1.44, true, true},
	"claude-sonnet-5.5": {14.4, 72, 0.72, true, true},

	// Google Gemini official USD list prices converted at the project rate (7.20).
	"gemini-3.8-flash": {5.4, 27, 0.54, true, true},

	// OpenAI official USD list prices converted at the project rate (7.20).
	"gpt-5.6-sol":   {28.8, 144, 2.88, true, true},
	"gpt-5.6-terra": {14.4, 86.4, 1.44, true, true},
	"gpt-5.6-luna":  {1.44, 8.64, 0.144, true, true},
	"gpt-6-astra":   {72, 360, 7.2, true, true},
	"gpt-6-sol":     {14.4, 72, 1.44, true, true},
	"gpt-6-luna":    {0.72, 3.6, 0.072, true, true},
	"gpt-6.1-sol":   {14.4, 72, 0.72, true, true},
}

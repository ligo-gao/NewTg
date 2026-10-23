export const vendors = [
  { id: "openai", name: "OpenAI", color: "#10a37f" },
  { id: "anthropic", name: "Anthropic", color: "#d97757" },
  { id: "google", name: "Google", color: "#4285f4" },
  { id: "deepseek", name: "DeepSeek", color: "#4d6bfe" },
  { id: "meta", name: "Meta", color: "#0866ff" },
  { id: "qwen", name: "阿里通义", color: "#ff6a00" },
] as const;

export type VendorId = (typeof vendors)[number]["id"];

export interface Model {
  id: string;
  name: string;
  vendor: VendorId;
  vendorName: string;
  context: string;
  inputPrice: string;
  outputPrice: string;
  latency: string;
  badge?: string;
}

export const models: Model[] = [
  {
    id: "gpt-4o",
    name: "GPT-4o",
    vendor: "openai",
    vendorName: "OpenAI",
    context: "128K",
    inputPrice: "$2.50",
    outputPrice: "$10.00",
    latency: "0.9s",
    badge: "热门",
  },
  {
    id: "claude-sonnet-4-5",
    name: "Claude Sonnet 4.5",
    vendor: "anthropic",
    vendorName: "Anthropic",
    context: "200K",
    inputPrice: "$3.00",
    outputPrice: "$15.00",
    latency: "1.1s",
    badge: "热门",
  },
  {
    id: "gemini-2-5-pro",
    name: "Gemini 2.5 Pro",
    vendor: "google",
    vendorName: "Google",
    context: "1M",
    inputPrice: "$1.25",
    outputPrice: "$10.00",
    latency: "1.3s",
    badge: "长上下文",
  },
  {
    id: "deepseek-v3-2",
    name: "DeepSeek-V3.2",
    vendor: "deepseek",
    vendorName: "DeepSeek",
    context: "128K",
    inputPrice: "$0.28",
    outputPrice: "$0.42",
    latency: "0.8s",
    badge: "高性价比",
  },
  {
    id: "llama-4-maverick",
    name: "Llama 4 Maverick",
    vendor: "meta",
    vendorName: "Meta",
    context: "1M",
    inputPrice: "$0.27",
    outputPrice: "$0.85",
    latency: "0.7s",
  },
  {
    id: "qwen3-max",
    name: "Qwen3-Max",
    vendor: "qwen",
    vendorName: "阿里通义",
    context: "256K",
    inputPrice: "$1.20",
    outputPrice: "$6.00",
    latency: "1.0s",
  },
];

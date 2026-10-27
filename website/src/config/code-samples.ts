export type CodeSample = {
  id: string;
  label: string;
  lang: string;
  code: string;
};

// Only the prompt text is localized; the snippets themselves stay identical across
// locales so we do not maintain five code samples per language.
const templates: CodeSample[] = [
  {
    id: "curl",
    label: "cURL",
    lang: "bash",
    code: `curl https://api.newtg.ai/v1/chat/completions \\
  -H "Authorization: Bearer $NEWTG_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{
    "model": "gpt-4o",
    "stream": true,
    "messages": [
      { "role": "user", "content": "{prompt}" }
    ]
  }'`,
  },
  {
    id: "python",
    label: "Python",
    lang: "python",
    code: `import os
from openai import OpenAI

client = OpenAI(
    base_url="https://api.newtg.ai/v1",
    api_key=os.environ["NEWTG_API_KEY"],
)

reply = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "{prompt}"}],
)`,
  },
  {
    id: "node",
    label: "Node.js",
    lang: "typescript",
    code: `import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "https://api.newtg.ai/v1",
  apiKey: process.env.NEWTG_API_KEY,
});

const reply = await client.chat.completions.create({
  model: "gpt-4o",
  stream: true,
  messages: [{ role: "user", content: "{prompt}" }],
});`,
  },
  {
    id: "ai-sdk",
    label: "AI SDK",
    lang: "typescript",
    code: `import { createOpenAI } from "@ai-sdk/openai";
import { generateText } from "ai";

const newtg = createOpenAI({
  baseURL: "https://api.newtg.ai/v1",
  apiKey: process.env.NEWTG_API_KEY,
});

const { text } = await generateText({
  model: newtg("gpt-4o"),
  prompt: "{prompt}",
});`,
  },
  {
    id: "langchain",
    label: "LangChain",
    lang: "python",
    code: `import os
from langchain_openai import ChatOpenAI

llm = ChatOpenAI(
    model="gpt-4o",
    base_url="https://api.newtg.ai/v1",
    api_key=os.environ["NEWTG_API_KEY"],
)

print(llm.invoke("{prompt}").content)`,
  },
];

export function codeSamples(prompt: string): CodeSample[] {
  return templates.map((sample) => ({
    ...sample,
    code: sample.code.replaceAll("{prompt}", prompt),
  }));
}

/**
 * Image generation helper for camelAI virtual AI bindings.
 *
 * The virtual `env.AI` binding only implements Cloudflare-compatible `run()`.
 * Call `env.CAMELAI.generateImage(prompt)` via the CAMELAI service binding
 * (`workers/camelai-service.ts`). This module implements the local shim logic.
 */

export interface GenerateImageOptions {
  prompt: string;
  /** Optional reference image (URL or `data:image/...` data URL) for style consistency. */
  referenceImageUrl?: string;
}

export interface GeneratedImage {
  dataUrl: string;
  index: number;
}

export interface GenerateImageResult {
  text: string | null;
  imageDataUrl: string | null;
  images: GeneratedImage[];
}

/** Minimal AI binding surface — matches Wrangler `Ai` / virtual binding `run()`. */
export type AiRunBinding = {
  run(model: string, input: unknown, options?: unknown): Promise<unknown>;
};

export function buildGenerateImageMessages(
  input: string | GenerateImageOptions,
): Array<{ role: "user"; content: string | Array<Record<string, unknown>> }> {
  const options = typeof input === "string" ? { prompt: input } : input;
  const prompt = options.prompt?.trim();
  if (!prompt) {
    throw new Error("generateImage requires a non-empty prompt");
  }

  const referenceImageUrl = options.referenceImageUrl?.trim();
  if (!referenceImageUrl) {
    return [{ role: "user", content: prompt }];
  }

  return [
    {
      role: "user",
      content: [
        {
          type: "image_url",
          image_url: { url: referenceImageUrl },
        },
        { type: "text", text: prompt },
      ],
    },
  ];
}

export function parseGenerateImageResponse(
  payload: unknown,
): GenerateImageResult {
  const empty: GenerateImageResult = {
    text: null,
    imageDataUrl: null,
    images: [],
  };
  if (!payload || typeof payload !== "object" || Array.isArray(payload)) {
    return empty;
  }

  const choices = (payload as { choices?: unknown }).choices;
  if (!Array.isArray(choices) || choices.length === 0) {
    return empty;
  }

  const firstChoice = choices[0];
  if (!firstChoice || typeof firstChoice !== "object") {
    return empty;
  }

  const message = (firstChoice as { message?: unknown }).message;
  if (!message || typeof message !== "object") {
    return empty;
  }

  const msg = message as Record<string, unknown>;
  const text =
    typeof msg.content === "string" && msg.content.trim()
      ? msg.content.trim()
      : null;

  const images: GeneratedImage[] = [];
  const rawImages = msg.images;
  if (Array.isArray(rawImages)) {
    for (const [fallbackIndex, item] of rawImages.entries()) {
      const dataUrl = extractGeneratedImageDataUrl(item);
      if (!dataUrl) continue;
      const index =
        item &&
        typeof item === "object" &&
        typeof (item as { index?: unknown }).index === "number"
          ? (item as { index: number }).index
          : fallbackIndex;
      images.push({ dataUrl, index });
    }
  }

  images.sort((a, b) => a.index - b.index);
  return {
    text,
    imageDataUrl: images[0]?.dataUrl ?? null,
    images,
  };
}

/**
 * Generate images via `auto_image`. Prefer this over `workers-ai-provider` /
 * `generateText()` with `auto_image` — images are dropped there.
 */
export async function generateImage(
  ai: AiRunBinding,
  input: string | GenerateImageOptions,
): Promise<GenerateImageResult> {
  const messages = buildGenerateImageMessages(input);
  const raw = await ai.run("auto_image", { messages });
  if (raw instanceof ReadableStream) {
    throw new Error("generateImage does not support streaming responses");
  }
  return parseGenerateImageResponse(raw);
}

function extractGeneratedImageDataUrl(item: unknown): string | null {
  if (!item || typeof item !== "object") return null;
  const imageUrl = (item as { image_url?: unknown }).image_url;
  if (!imageUrl || typeof imageUrl !== "object") return null;
  const url = (imageUrl as { url?: unknown }).url;
  return typeof url === "string" && url.trim() ? url.trim() : null;
}

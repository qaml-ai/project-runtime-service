import { WorkerEntrypoint } from "cloudflare:workers";
import {
  generateImage,
  type GenerateImageOptions,
  type GenerateImageResult,
} from "./camelai-ai";

interface LocalCamelAiEnv {
  AI: Ai;
}

/**
 * Local CAMELAI shim used by the starter template.
 * Deploy pipeline rewrites this binding to the platform's internal CamelAiService.
 */
export class LocalCamelAiService extends WorkerEntrypoint<LocalCamelAiEnv> {
  async generateImage(
    input: string | GenerateImageOptions,
  ): Promise<GenerateImageResult> {
    return generateImage(this.env.AI, input);
  }
}

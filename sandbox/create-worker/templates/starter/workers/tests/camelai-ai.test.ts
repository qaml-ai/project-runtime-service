import { describe, expect, it } from "vitest";
import {
  buildGenerateImageMessages,
  generateImage,
  parseGenerateImageResponse,
} from "../camelai-ai";

describe("buildGenerateImageMessages", () => {
  it("builds a text-only user message from a string prompt", () => {
    expect(buildGenerateImageMessages("a red cube")).toEqual([
      { role: "user", content: "a red cube" },
    ]);
  });

  it("includes a reference image when provided", () => {
    const messages = buildGenerateImageMessages({
      prompt: "same style",
      referenceImageUrl: "data:image/png;base64,abc",
    });
    expect(messages[0]?.content).toEqual([
      {
        type: "image_url",
        image_url: { url: "data:image/png;base64,abc" },
      },
      { type: "text", text: "same style" },
    ]);
  });
});

describe("parseGenerateImageResponse", () => {
  it("extracts text and image data URLs", () => {
    const result = parseGenerateImageResponse({
      choices: [
        {
          message: {
            content: "Here is your image.",
            images: [
              {
                index: 0,
                image_url: { url: "data:image/png;base64,abc" },
              },
            ],
          },
        },
      ],
    });
    expect(result.text).toBe("Here is your image.");
    expect(result.imageDataUrl).toBe("data:image/png;base64,abc");
    expect(result.images).toEqual([
      { index: 0, dataUrl: "data:image/png;base64,abc" },
    ]);
  });
});

describe("generateImage", () => {
  it("calls auto_image and parses the gateway payload", async () => {
    const ai = {
      run: async (model: string, input: unknown) => {
        expect(model).toBe("auto_image");
        expect(input).toEqual({
          messages: [{ role: "user", content: "draw a star" }],
        });
        return {
          choices: [
            {
              message: {
                content: "done",
                images: [
                  {
                    index: 0,
                    image_url: { url: "data:image/png;base64,star" },
                  },
                ],
              },
            },
          ],
        };
      },
    };

    const result = await generateImage(ai, "draw a star");
    expect(result.imageDataUrl).toBe("data:image/png;base64,star");
  });
});

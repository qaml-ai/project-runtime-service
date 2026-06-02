import { z } from "zod";

/**
 * Shared Zod schemas - import in both routes and Durable Objects
 * to enforce consistent types everywhere.
 */

export const ContactSchema = z.object({
  id: z.number(),
  name: z.string(),
  email: z.string().email(),
  created_at: z.string(),
});

export const CreateContactInputSchema = z.object({
  name: z.string().min(1, "Name is required"),
  email: z.string().email("Invalid email address"),
});

// TypeScript types derived from schemas
export type Contact = z.infer<typeof ContactSchema>;
export type CreateContactInput = z.infer<typeof CreateContactInputSchema>;

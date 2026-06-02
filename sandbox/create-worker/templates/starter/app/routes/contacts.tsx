import type { Route } from "./+types/contacts";
import { Form, useNavigation } from "react-router";
import { CreateContactInputSchema, type Contact } from "~/schemas/contact";

/**
 * Example route showing key React Router 7 + Cloudflare patterns:
 * - Accessing DO via context.cloudflare.env
 * - RPC method calls on DO stubs
 * - Zod validation in actions
 * - HydrateFallback for loading states
 */

// Loader: Fetch data on the server
// Access Cloudflare bindings via context.cloudflare.env
export async function loader({ context }: Route.LoaderArgs) {
  // Get a DO stub using a fixed ID (or derive from user/session)
  const id = context.cloudflare.env.EXAMPLE_DO.idFromName("global");
  const stub = context.cloudflare.env.EXAMPLE_DO.get(id);

  // Call RPC methods directly on the stub (not fetch!)
  const contacts = await stub.listContacts();

  return { contacts };
}

// Action: Handle form submissions
export async function action({ request, context }: Route.ActionArgs) {
  const formData = await request.formData();
  const intent = formData.get("intent");

  const id = context.cloudflare.env.EXAMPLE_DO.idFromName("global");
  const stub = context.cloudflare.env.EXAMPLE_DO.get(id);

  if (intent === "create") {
    // Validate input with Zod schema (shared with DO)
    const result = CreateContactInputSchema.safeParse({
      name: formData.get("name"),
      email: formData.get("email"),
    });

    if (!result.success) {
      return { error: result.error.errors[0].message };
    }

    await stub.createContact(result.data);
  }

  if (intent === "delete") {
    const contactId = Number(formData.get("id"));
    await stub.deleteContact(contactId);
  }

  return { ok: true };
}

// HydrateFallback: Shown during client-side hydration
// Use this instead of useNavigation for initial page load
export function HydrateFallback() {
  return (
    <div className="p-8">
      <h1 className="text-2xl font-bold mb-4">Contacts</h1>
      <div className="animate-pulse space-y-2">
        <div className="h-4 bg-gray-200 rounded w-1/4"></div>
        <div className="h-4 bg-gray-200 rounded w-1/3"></div>
        <div className="h-4 bg-gray-200 rounded w-1/4"></div>
      </div>
    </div>
  );
}

export default function Contacts({ loaderData, actionData }: Route.ComponentProps) {
  const { contacts } = loaderData;
  const navigation = useNavigation();
  const isSubmitting = navigation.state === "submitting";

  return (
    <div className="p-8 max-w-2xl mx-auto">
      <h1 className="text-2xl font-bold mb-6">Contacts</h1>

      {/* Create form */}
      <Form method="post" className="mb-8 space-y-4">
        <input type="hidden" name="intent" value="create" />
        <div>
          <input
            name="name"
            placeholder="Name"
            required
            className="border rounded px-3 py-2 w-full"
          />
        </div>
        <div>
          <input
            name="email"
            type="email"
            placeholder="Email"
            required
            className="border rounded px-3 py-2 w-full"
          />
        </div>
        {actionData?.error && (
          <p className="text-red-500 text-sm">{actionData.error}</p>
        )}
        <button
          type="submit"
          disabled={isSubmitting}
          className="bg-blue-500 text-white px-4 py-2 rounded disabled:opacity-50"
        >
          {isSubmitting ? "Adding..." : "Add Contact"}
        </button>
      </Form>

      {/* Contacts list */}
      <ul className="space-y-2">
        {contacts.map((contact: Contact) => (
          <li
            key={contact.id}
            className="flex items-center justify-between p-3 bg-gray-50 rounded"
          >
            <div>
              <div className="font-medium">{contact.name}</div>
              <div className="text-sm text-gray-500">{contact.email}</div>
            </div>
            <Form method="post">
              <input type="hidden" name="intent" value="delete" />
              <input type="hidden" name="id" value={contact.id} />
              <button
                type="submit"
                className="text-red-500 hover:text-red-700 text-sm"
              >
                Delete
              </button>
            </Form>
          </li>
        ))}
        {contacts.length === 0 && (
          <li className="text-gray-500 text-center py-4">No contacts yet</li>
        )}
      </ul>
    </div>
  );
}

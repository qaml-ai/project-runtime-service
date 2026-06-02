import { type RouteConfig, index, route } from "@react-router/dev/routes";

export default [
  index("routes/home.tsx"),
  route("contacts", "routes/contacts.tsx"),
  // Uncomment to enable AI chat route (requires Chat + ChatSessionsDO and agent routing)
  // route("chat", "routes/chat.tsx"),
] satisfies RouteConfig;

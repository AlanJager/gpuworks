# ADR 0081: Plugin Routes Authorized by Role: a Route Declares `access: role`, and Pushed Tables Grant Roles and Actions

Date: 2026-11-22 · Status: Accepted, not yet implemented (adds to [ADR 0071](0071-installable-extensions.md) decisions 8, 9 and 11)

## Background

In the request pipeline of plugin routes there is an authorization point after authentication (`authorizeDynamic`, step 4 in [Extension Points](../dev/extensions.md)), and today it always passes. Every route declares an `action` in the signed manifest (`<package id>.<verb>`, "the action name used for authorization"), and the principal forwarded to the plugin carries `route_action`, but nothing in the core compares it with anything.

All that can stop a caller today is the route's access level: `admin`, `admin_session`, `member`, `public`. The core has only two user roles, `admin` and `user` (`users.role`), and the principal's `roles` is a list holding that one role. So "only some of the signed-in users may call this route" cannot be expressed today; each plugin has to read the principal and write the check itself.

ADR 0071 already left the vocabulary for this: "custom roles" is among the capabilities of T2 packages; pushed tables of the grant class are "exclusive by key or a union by role key, and a conflict is refused"; the table kinds reserve `roles.core_actions` (roles to core actions, with no schema yet); and the uninstall preview has `UsersAffected`, with the note that "no table kind of schema 1 binds users". What is missing is the concrete tables and the enforcement rule.

One constraint decides the shape of the design: with only the two roles `admin` and `user`, everything a "role to action" table can express is already expressed by access levels. Authorization by action has content only once a user can hold a role beyond those two, so where roles come from and how actions are granted have to be decided together.

## Decision

1. **A route declares that roles govern it with an access level: `access: role`.** It sits between `member` and `admin`: it needs a signed-in user or a platform token (a read token can only read), and one of the caller's roles must have been granted this route's `action`. A platform administrator (`users.role` is `admin`) always passes. One more access level changes the outcome of enforcement, so this is manifest schema 2; an older core refuses to install a schema 2 package instead of treating `role` as some other level.
2. **Whether a route is governed is decided by its own signed manifest; tables only grant and never draw the boundary.** When no table grants the action of a `role` route, it is open only to platform administrators. So when the granting package goes down, is uninstalled, or has its files damaged, the result only tightens, in line with ADR 0071's "deleting or damaging package files never relaxes a restriction". The opposite design (an action becomes governed once some table mentions it) would reopen the route to every signed-in user when the granting package disappears, so it is not taken.
3. **Two new pushed tables of the grant class, both privileged: only a certificate grant allows pushing them.**
   - `roles.bindings`: a row is `{user_id, roles}`, binding a core user id to role names. Rows reference core ids, not usernames.
   - `roles.plugin_actions`: a row is `{role, actions}`, binding a role name to actions of plugin routes. An action must be one declared in the manifest of an installed package, on a route whose access is `role`; any other row makes the whole table refused, not silently dropped.
   Both tables carry `down_grace_s` and have a row limit; above it the whole table is refused.
4. **A role name is one word, exclusive by key.** Role names keep the grammar the principal already has (`[a-z][a-z0-9_]{0,31}`), so the principal's schema does not change. The first package to bind or grant a role name owns it; when another package pushes a role of the same name, the whole table is refused with a conflict. `admin` and `user` are the core's names, and no package may push them. A site may have several packages that push roles, each with its own names.
5. **A user's effective roles = the core role ∪ the pushed roles.** The principal's `roles` carries the effective roles, sorted; `platform_admin` and `local_admin` still read only `users.role` and `users.local_admin`, and a pushed role can never make a user a platform administrator. A platform token takes the roles of the user it belongs to.
6. **Enforced at the existing point, and nowhere outside the hot path.** For a route whose access is `role`, `authorizeDynamic` reads a snapshot (one atomic read and a map lookup, no Store call, no I/O, no plugin call) and on refusal answers 403 with the coded `PLUGIN_ACTION_FORBIDDEN{action}`. When the route's feature key is hidden, the answer is still 404 first: the 403 of a forbidden action must not reveal that a hidden feature exists. Routes of the other access levels do not read this snapshot and behave byte for byte as today.
7. **Going down is handled as for the grant class.** Once a package that pushes roles has been down longer than the grace, its bindings and grants are dropped: the `role` routes granted only through it go back to being open to platform administrators alone. Both tables are read at startup before the router is built, so after a core restart, and before any plugin comes up, enforcement follows the last state.
8. **The uninstall preview names the people affected.** For a package that pushed `roles.bindings`, `UsersAffected` in the uninstall preview is the number of users it bound roles to.
9. **Core routes are outside this ADR.** `roles.core_actions` stays reserved and still has no schema. When it lands, it uses the same role names and the same `roles.bindings`.

## Implementation Order

The code starts only when the first package that pushes roles needs it, and until then no wire format is frozen for it; the three steps can then be merged separately or done together, each with contract tests against the fake plugin process (ADR 0071 decision 21).

1. Manifest schema 2 and `access: role`; `authorizeDynamic` enforces with an empty snapshot (`role` routes are open only to platform administrators); the error code and the docs. After this step the visible behaviour is the same as declaring the route `admin`; what it does is settle the wire format.
2. `roles.bindings`: the table, persistence, effective roles in the principal, the uninstall preview.
3. `roles.plugin_actions`: the table, the snapshot, passing by role. Only at this step does authorization by role become visible to users.

## Trade-offs

- **Pushed tables vs a decision function passed in by a compiled module.** A decision function is the least work, but it is a second channel of authorization: it bypasses the merge rules of pushed tables, the degradation on going down, and the uninstall preview, and nothing implements it today. Under ADR 0071, T1 is only composition inside the core, and grants come from an authority's push.
- **A new access level vs a boolean field on the route.** Both need a new manifest schema. The access level is already the one declaration of "who may call", read in one place; a boolean field would also let meaningless combinations such as `admin` plus "governed by roles" be written.
- **Platform administrators always pass vs administrators are bound by roles too.** An administrator can already uninstall packages, override restrictions and change any user, so stopping them from calling one route blocks nothing, yet it would leave `role` routes unusable by anyone when no role package is installed. Data that "even administrators must not see" is for the plugin to decide from the principal.
- **A role name is one word vs `<package id>.<role>`.** Namespaced names cannot collide, but the principal's `roles` is validated as "words" today, and changing that is principal schema 2, which every published plugin would have to follow. Exclusivity by key with refusal on conflict trades one check at installation for an unchanged wire format.
- **Only grants, no deny rows.** Deny rows would need an order between allow and deny, and the merge would no longer be a union. To take a permission back, remove the grant.

## Not Done

- No role management page or role table in the core: roles are defined and pushed by packages, and the core only enforces.
- No data scope: `scope_projects` in the principal is not filled by this ADR.
- A site administrator cannot change the access level of a plugin route in the core; tightening a third-party package's route is a decision of another class (the restriction class).

## Implementation Status

- Not started, waiting for the first package that pushes roles. Today `authorizeDynamic` always passes; apart from `roles.core_actions` there is no table kind about roles; the principal's `roles` holds only the core role.

<!-- translation-source-sha256: 83674b5a9174c7ba7e0326868c92616a78b2389c64635e51d29e53a907188a7f -->

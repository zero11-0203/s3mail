# AWS architecture diagram

[Documentation index](../README.md) · [Architecture](../architecture.md)

[View or edit the diagram](aws-mail-system.drawio.svg)

`aws-mail-system.drawio.svg` is the single maintained AWS overview. It renders as an image in Markdown and contains the editable draw.io diagram. There is no separate source/export pair to keep synchronized.

## Scope

Show AWS services, external clients/relay, primary message and request paths, authoritative storage, asynchronous work and the deferred browser deployment. Mark the design as proposed until implementation evidence justifies a status change. Show aerc and the helpers as the terminal access path, and the browser webmail and the mail agent as separately deployed parts. Keep phases, priorities, stages and gates out of the diagram's labels; they live in the [roadmap](../roadmap.md).

The original-mail S3 bucket retains all versions permanently; relevant roles have no delete permissions. Temporary uploads and derived objects use separate disposable storage. There is no backup bucket, scheduled export job or DynamoDB PITR in the current personal-use scope. The repeated DynamoDB symbol references the same mail table. Shared-service access and supporting operations are labeled rather than drawing every connection. The bounce path groups its gateway, receiver, queue and worker. Exact permissions, limits and recovery procedures remain in their owning specs; this diagram does not replace those contracts.

## When to update

Update the diagram in the same change when adding, removing or changing a service, integration, data flow, authentication/authorization boundary, hosting choice or deployment stage. Check it when changing the architecture, relevant specs or future infrastructure code. Record “no diagram impact” with a reason when review finds no visual change necessary.

Keep the diagram's date current when its design changes. Keep the architecture page and its component table consistent with the diagram. Topic-specific sequence/flow diagrams can remain in specs; do not create another copy of the AWS overview.

## Editing and verification

1. Open `aws-mail-system.drawio.svg` in draw.io Desktop or an editor with draw.io integration. It loads the embedded diagram as editable shapes.
2. Change the diagram using the owning specs as the authority. Keep AWS service icons, readable labels and clear arrow directions. Dashed connectors denote the separately deployed webmail and agent paths.
3. Export SVG with **Include a copy of my diagram** enabled and replace this same file. For scripted editing, the SVG root's `content` attribute contains the embedded `mxfile` XML; edit a temporary `.drawio` copy and export it with `--embed-diagram` (`-e`). Do not edit only the rendered SVG paths.
4. Reopen the resulting SVG in draw.io to confirm editability, and render a preview to inspect clipping, overlapping connectors, text and the webmail and agent boundaries. Check the image link in `docs/architecture.md`.
5. State the diagram impact in the commit message (external contributions complete the pull-request checklist). Review changes to both the embedded model and the visible image; an updated date alone does not establish correctness.

The diagram is maintained with repository changes, not synchronized automatically from a live AWS account. Deployed resources exist for validation, but the diagram shows the design; check deployed state with Terraform, not the diagram.

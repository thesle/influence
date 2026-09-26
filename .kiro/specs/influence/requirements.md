# Requirements Document

## Introduction

Influence is a self-hostable, lightweight documentation platform (a Confluence alternative) targeted at non-profit organizations that need an inexpensive, easy-to-use knowledge system. The backend is written in Go, the frontend in SvelteKit, and the platform runs standalone on a Linux VM (Ubuntu and Fedora as primary targets) with SSL support. Content and identity are stored in SQLite databases.

The platform is multitenant. A single central database manages user identity, while each tenant (organization) owns a separate SQLite database containing that tenant's content. This isolation supports a key goal: a volunteer who stops working with an organization can hand over the organization's SQLite content database to a successor who can then self-host it.

The platform taxonomy is: **Influence** (Platform) > **Spheres** (Workspaces/Domains) > **Facets** (Category Hubs, arranged in a hierarchy) > **Polygons** (Articles/Documents, the fundamental content units). Polygons may be markdown pages, folders, tabular data, or freehand/chart whiteboards. Content is authored in markdown augmented with `@` commands and rendered as richly formatted pages, with an editing mode that converts back to markdown for easier editing.

This document defines the functional and non-functional requirements using EARS patterns and INCOSE quality rules.

## Glossary

- **Influence**: The platform / central knowledge ecosystem software, comprising the Go backend and SvelteKit frontend.
- **Influence_System**: The running server application (backend + frontend) that serves requests. Used as the responsible system in most requirements.
- **Central_Directory**: The single SQLite database that stores platform-wide user identity, credentials, and tenant registry information.
- **Tenant**: An organization whose content is stored in its own isolated database. Also referred to as an org.
- **Tenant_Database**: A SQLite database that stores all content and content-related metadata for exactly one Tenant.
- **Tenant_Salt**: A cryptographic salt value unique to a single Tenant, used for encrypting Sensitive_Data within that Tenant.
- **User**: An authenticated person who accesses Influence.
- **Group**: A named collection of Users. A Group can be granted access to one or more Spheres.
- **Admin_Group**: A reserved Group whose members can manage Users, Groups, and access, and can view all Spheres within a Tenant.
- **Sphere**: A top-level workspace/domain that separates content into a distinct topic (analogous to a Confluence Space).
- **Circled_Sphere**: A Sphere that a User has marked to keep pinned at the top of that User's Sphere list.
- **Facet**: A structural category hub within a Sphere, arranged in a hierarchy, that organizes Polygons.
- **Polygon**: The fundamental content unit within a Sphere. A Polygon is one of four types: Markdown_Page, Folder_Polygon, Tabular_Polygon, or Whiteboard_Polygon.
- **Markdown_Page**: A Polygon whose content is authored in markdown with `@` commands.
- **Folder_Polygon**: A Polygon that organizes other content.
- **Tabular_Polygon**: A Polygon that holds tabular data.
- **Whiteboard_Polygon**: A Polygon that holds freehand or chart-style whiteboard content.
- **At_Command**: An in-editor command prefixed with `@` that inserts dynamic or structured content. Defined commands are `@link`, `@heading`, `@toc`, and `@children`.
- **Markdown_Editor**: The authoring component that accepts markdown and `@` commands and provides rich rendering and editing.
- **Rich_Renderer**: The component that renders markdown and `@` commands into a formatted page.
- **Sensitive_Data**: A span of Polygon content that a User has marked for encryption and obfuscation.
- **Obfuscation_Token**: The stored placeholder that references Sensitive_Data by identifier, in the form `|encrypt|{id}|`.
- **Reveal_Permission**: A per-Sphere permission that authorizes a User to reveal Sensitive_Data within that Sphere.
- **Record_ID**: A UUID or Short UUID that uniquely and stably identifies a record (such as a Polygon) independent of its display name.
- **Bookmark**: A User-specific marker on a Polygon that adds the Polygon to that User's bookmark list.
- **PDF_Export**: A generated PDF document containing only rendered Polygon content, excluding toolbars and menus.
- **Search_Service**: The component that finds Polygons by related content.
- **Cross_Refraction**: An inline cross-Sphere link between Polygons.
- **Makefile**: The project build file providing dev, build, install, and uninstall targets.
- **Systemd_Service**: A systemd unit that runs the Influence_System as a managed background service on a Linux host.
- **Data_Directory**: The filesystem directory where the Central_Directory and all Tenant_Database files are stored, defaulting to /var/lib/influence.
- **First_Run_State**: The condition in which a Tenant's Tenant registry exists but the Central_Directory contains no User assigned to that Tenant who is a member of that Tenant's Admin_Group. In this state the Tenant has no administrator who can log in.
- **Bootstrap_Admin**: The first administrator User created for a Tenant through the first-run setup flow, established as a member of that Tenant's Admin_Group.
- **Setup_Screen**: The first-run user interface shown in place of the login screen while a Tenant is in First_Run_State, which collects the Bootstrap_Admin username and password.
- **Admin_API**: The set of HTTP endpoints, restricted to Admin_Group members, for managing Users, Groups, and per-Sphere Group access within the caller's Tenant.
- **Identity_Endpoint**: The HTTP endpoint that returns the authenticated User's own identity and effective permissions for the current session.
- **Two_Factor_Authentication**: A second authentication step, in addition to the password, in which a User provides a time-based one-time password (TOTP) generated by an authenticator app. Abbreviated 2FA.
- **TOTP_Secret**: The per-User shared secret from which time-based one-time passwords are derived, stored in the Central_Directory.
- **TOTP_Provisioning_URI**: The `otpauth://` URI, encodable as a QR code, that an authenticator app scans to enrol a User's TOTP_Secret.
- **Two_Factor_Policy**: A per-Tenant setting, controlled by the Admin_Group, that makes Two_Factor_Authentication either optional or required for that Tenant's Users.
- **Recovery_Code**: A single-use backup code, issued at Two_Factor_Authentication enrolment, that a User can present in place of a TOTP when the authenticator device is unavailable.

## Requirements

### Requirement 1: Multitenancy and Central Directory

**User Story:** As a platform operator, I want user identity managed centrally while each organization's content lives in its own database, so that organizations remain isolated and portable.

#### Acceptance Criteria

1. THE Influence_System SHALL store platform-wide User identity, credentials, and Tenant registry entries in the Central_Directory.
2. THE Influence_System SHALL store all content and content metadata for each Tenant exclusively in that Tenant's own Tenant_Database, with no content or content metadata stored in the Central_Directory.
3. THE Influence_System SHALL associate each User with exactly one Tenant.
4. WHEN a User authenticates successfully, THE Influence_System SHALL determine the single Tenant associated with that User and route all subsequent content requests from that User to the associated Tenant's Tenant_Database.
5. THE Influence_System SHALL restrict each content request to data contained in the Tenant_Database of the requesting User's authenticated Tenant.
6. IF a content request references data that resides outside the requesting User's authenticated Tenant_Database, THEN THE Influence_System SHALL reject the request, return an error response indicating the data is not accessible, and leave all Tenant_Database data unchanged.
7. WHEN a new Tenant is created, THE Influence_System SHALL create a dedicated Tenant_Database for that Tenant and record a corresponding Tenant registry entry in the Central_Directory.
8. IF creation of the dedicated Tenant_Database fails during Tenant creation, THEN THE Influence_System SHALL abort the Tenant creation, remove any partially created Tenant registry entry from the Central_Directory, and return an error response indicating the Tenant was not created.

### Requirement 2: Tenant Data Portability

**User Story:** As a volunteer administrator, I want to hand over an organization's content database, so that a successor can self-host it after I leave.

#### Acceptance Criteria

1. THE Influence_System SHALL store each Tenant's content, including embedded images and content assets, within that Tenant's single Tenant_Database file with no dependency on an external content store.
2. WHERE an operator requests a Tenant export, THE Influence_System SHALL produce a copy of that Tenant's Tenant_Database file including the Tenant_Salt and key material required to decrypt that Tenant's encrypted content.
3. THE Influence_System SHALL exclude other Tenants' content, images, and salts from any single Tenant_Database file.
4. IF a Tenant export cannot be produced because the Tenant_Database file is missing, locked, or corrupt, THEN THE Influence_System SHALL abort the export and return an error indicating the export failed.

### Requirement 3: Authentication

**User Story:** As a User, I want to log in securely, so that only authorized people can access organization content.

#### Acceptance Criteria

1. WHEN a User submits valid credentials, THE Influence_System SHALL establish an authenticated session for that User.
2. IF a User submits invalid credentials, THEN THE Influence_System SHALL deny access and return an authentication error.
3. IF a User submits invalid credentials 5 consecutive times, THEN THE Influence_System SHALL lock further authentication attempts for that User for 15 minutes.
4. THE Influence_System SHALL store User credentials in the Central_Directory using a one-way password hash.
5. WHEN a User logs out, THE Influence_System SHALL terminate that User's authenticated session and deny subsequent requests made with that session.
6. THE Influence_System SHALL expire an authenticated session after 30 minutes of inactivity or 24 hours after establishment, whichever occurs first.

### Requirement 4: User and Group Management

**User Story:** As an administrator, I want to manage users and groups and assign group access to Spheres, so that the right people can reach the right content.

#### Acceptance Criteria

1. THE Influence_System SHALL require each User to be a member of at least one Group at all times, and SHALL reject any operation that would leave a User with zero Group memberships with an error indicating that at least one Group membership is required.
2. THE Influence_System SHALL allow a Group to be granted access to zero or more Spheres, up to the total number of Spheres within the Group's Tenant.
3. WHEN a Group is granted access to a Sphere, THE Influence_System SHALL grant every current member of that Group the access level (read or write) configured for that Group on that Sphere.
4. WHEN a Group is granted access to a Sphere with Reveal_Permission enabled, THE Influence_System SHALL permit members of that Group to view the sensitive data within that Sphere; otherwise THE Influence_System SHALL withhold the sensitive data from those members while still permitting access to non-sensitive content.
5. IF a User is not a member of any Group with access to a requested Sphere, THEN THE Influence_System SHALL deny that User access to the Sphere and return an error indicating that the User lacks access to the requested Sphere.
6. WHEN a Group loses access to a Sphere, THE Influence_System SHALL revoke that Sphere's access for every member of that Group unless the member retains access through another Group.
7. WHERE a User is a member of the Admin_Group, THE Influence_System SHALL grant that User read and write access to all Spheres within the User's Tenant, including sensitive data governed by Reveal_Permission.

### Requirement 5: Administrative Capabilities

**User Story:** As an Admin_Group member, I want dedicated administration pages, so that I can manage users, groups, and per-Sphere access.

#### Acceptance Criteria

1. WHERE a User is a member of the Admin_Group, THE Influence_System SHALL provide administration pages for creating, editing, and deactivating Users within the Admin_Group member's Tenant.
2. WHERE a User is a member of the Admin_Group, THE Influence_System SHALL provide administration pages for creating and editing Groups within the Admin_Group member's Tenant.
3. WHERE a User is a member of the Admin_Group, THE Influence_System SHALL provide administration pages for assigning Group access to each Sphere, including selection of access level (read or write) and Reveal_Permission for that Group on that Sphere.
4. WHEN an Admin_Group member deactivates a User, THE Influence_System SHALL deny that deactivated User access to all Spheres and administration pages while retaining that User's Group memberships and account record.
5. IF a User who is not a member of the Admin_Group requests an administration page, THEN THE Influence_System SHALL deny access to that page and return an error indicating that administrative privileges are required.

### Requirement 6: Spheres

**User Story:** As a User, I want content separated into Spheres, so that distinct topics stay organized.

#### Acceptance Criteria

1. THE Influence_System SHALL organize content into Spheres, where each Sphere represents a distinct topic within a Tenant.
2. WHEN a User with create permission creates a Sphere with a name of 1 to 100 characters, THE Influence_System SHALL store the Sphere in the User's Tenant_Database.
3. THE Influence_System SHALL identify each Sphere by a Record_ID.
4. IF a User attempts to create a Sphere with an empty name, a name exceeding 100 characters, or a name duplicating an existing Sphere in the Tenant, THEN THE Influence_System SHALL reject the request and return a validation error.
5. WHEN an authorized User deletes a Sphere, THE Influence_System SHALL delete the Sphere together with all Facets and Polygons it contains.

### Requirement 7: Circling and Ordering Spheres

**User Story:** As a User, I want to pin and reorder my favorite Spheres, so that I can reach frequently used Spheres quickly.

#### Acceptance Criteria

1. WHEN a User circles a Sphere, THE Influence_System SHALL add that Sphere to that User's Circled_Sphere list.
2. IF a User circles a Sphere already in that User's Circled_Sphere list, THEN THE Influence_System SHALL leave the list unchanged.
3. THE Influence_System SHALL display each User's Circled_Sphere list above that User's remaining accessible Spheres.
4. WHEN a User reorders that User's Circled_Sphere entries, THE Influence_System SHALL persist and apply the User-specified order for that User.
5. THE Influence_System SHALL display accessible Spheres that are not circled in ascending order by Sphere name using case-insensitive comparison, with Record_ID as a tie-break.
6. WHEN a User un-circles a Sphere, THE Influence_System SHALL remove that Sphere from that User's Circled_Sphere list and return it to that User's alphabetical list.

### Requirement 8: Facets Hierarchy

**User Story:** As a User, I want Facets arranged in a hierarchy within a Sphere, so that Polygons are structured by category.

#### Acceptance Criteria

1. THE Influence_System SHALL organize Polygons within a Sphere under Facets arranged in a hierarchy.
2. THE Influence_System SHALL allow a Facet to contain child Facets.
3. THE Influence_System SHALL identify each Facet by a Record_ID.
4. WHEN a User with create permission creates a Facet with a name of 1 to 100 characters within a Sphere, THE Influence_System SHALL store the Facet in that Sphere within the Tenant_Database.
5. IF a User attempts to create a Facet with an empty name or a name exceeding 100 characters, THEN THE Influence_System SHALL reject the request and return a validation error.
6. WHEN an authorized User deletes a Facet, THE Influence_System SHALL delete the Facet together with all descendant Facets and contained Polygons.

### Requirement 9: Facet Hierarchy Integrity

**User Story:** As a User, I want the Facet hierarchy to remain a valid tree, so that navigation does not break.

#### Acceptance Criteria

1. IF a User attempts to set a Facet's parent to itself or to one of its own descendants, THEN THE Influence_System SHALL reject the change, return a hierarchy error, and leave the hierarchy unchanged.
2. FOR ALL Facets within a Sphere, THE Influence_System SHALL maintain a hierarchy in which each Facet has at most one parent and no Facet is its own ancestor.
3. IF a User attempts to set a Facet's parent to a Facet in a different Sphere, THEN THE Influence_System SHALL reject the change and return a hierarchy error.

### Requirement 10: Polygon Types

**User Story:** As a User, I want different kinds of Polygons, so that I can capture pages, folders, tables, and whiteboards.

#### Acceptance Criteria

1. THE Influence_System SHALL support exactly four Polygon types: Markdown_Page, Folder_Polygon, Tabular_Polygon, and Whiteboard_Polygon.
2. WHEN an authorized User creates a Polygon, THE Influence_System SHALL record the selected Polygon type as one of the four supported types.
3. IF a Polygon creation request specifies a Polygon type that is not one of the four supported types, THEN THE Influence_System SHALL reject the request without creating a Polygon and SHALL return an indication that the type is invalid.
4. THE Influence_System SHALL identify each Polygon by a Record_ID that is unique within the Tenant_Database.
5. WHEN an authorized User creates a Polygon within a Sphere, THE Influence_System SHALL store the Polygon in that Sphere within the Tenant_Database.
6. IF storing a newly created Polygon in the Tenant_Database fails, THEN THE Influence_System SHALL not persist the Polygon and SHALL return an indication that creation failed, leaving the target Sphere unchanged.
7. THE Influence_System SHALL allow a Folder_Polygon to contain other Polygons, including other Folder_Polygons.

### Requirement 11: Markdown Authoring with At-Commands

**User Story:** As an author, I want markdown pages with `@` commands, so that I can insert links and structure quickly.

#### Acceptance Criteria

1. THE Markdown_Editor SHALL store Markdown_Page content as markdown text.
2. WHEN a User enters `@link`, THE Markdown_Editor SHALL present a selectable list of the other Polygons within the current Tenant_Database available to link to.
3. IF a User enters `@link` and no other Polygons are available to link to, THEN THE Markdown_Editor SHALL present an empty selectable list with an indication that no linkable Polygons exist.
4. WHEN a User enters `@heading`, THE Markdown_Editor SHALL present a selectable list of markdown heading styles.
5. WHEN a User enters `@toc`, THE Rich_Renderer SHALL generate a table of contents composed of the current page's markdown headings.
6. IF a User enters `@toc` and the current page contains no markdown headings, THEN THE Rich_Renderer SHALL generate an empty table of contents.
7. WHEN a User enters `@children`, THE Rich_Renderer SHALL generate a list of all child Polygons of the current Polygon.
8. IF a User enters `@children` and the current Polygon has no child Polygons, THEN THE Rich_Renderer SHALL generate an empty list.
9. WHEN the set of headings on a page changes or the set of child Polygons of the current Polygon changes, THE Rich_Renderer SHALL regenerate any `@toc` and `@children` content on that page to reflect the current structure.
10. THE Markdown_Editor SHALL provide an on-screen icon for each At_Command in addition to the typed command.

### Requirement 12: Rich Rendering and Editing

**User Story:** As a less-technical author, I want content rendered nicely and easy to edit, so that I do not need to read raw markdown.

#### Acceptance Criteria

1. WHEN a User finishes editing a section, THE Rich_Renderer SHALL render that section's markdown into a formatted display.
2. WHEN a User selects a rendered section for editing, THE Markdown_Editor SHALL present that section's underlying markdown text for editing.
3. FOR ALL Markdown_Page content, WHEN a User renders a section and then selects that section for editing without modifying it, THE Markdown_Editor SHALL present markdown text that is byte-for-byte identical to that section's original stored markdown text (round-trip property).
4. IF a User cancels editing a section without confirming changes, THEN THE Markdown_Editor SHALL retain that section's original stored markdown text unchanged.

### Requirement 13: External Links

**User Story:** As an author, I want to link to external sites, so that I can reference outside resources.

#### Acceptance Criteria

1. WHEN a User inserts an external link with a URL of 1 to 2048 characters that begins with a supported web scheme, THE Markdown_Editor SHALL store the external URL within the Polygon content.
2. IF a User inserts an external link whose URL is empty, exceeds 2048 characters, or does not begin with a supported web scheme, THEN THE Markdown_Editor SHALL reject the link, preserve the existing Polygon content unchanged, and display an indication that the URL is invalid.
3. WHEN a Polygon containing a stored external link is rendered, THE Rich_Renderer SHALL display the external link as an activatable link that resolves to the stored external URL.

### Requirement 14: Cross-Sphere Linking (Cross-Refraction)

**User Story:** As an author, I want inline links between Polygons across Spheres, so that related content stays connected.

#### Acceptance Criteria

1. WHEN a User creates a Cross_Refraction link, THE Influence_System SHALL store the link as a reference to the target Polygon's Record_ID.
2. WHEN a Polygon containing a Cross_Refraction link is rendered and the target Polygon exists and is accessible to the viewer, THE Rich_Renderer SHALL resolve the Record_ID to the target Polygon's current name and display the link as activatable.
3. IF a Polygon containing a Cross_Refraction link is rendered and the target Polygon's Record_ID cannot be resolved because the target Polygon has been deleted, THEN THE Rich_Renderer SHALL display the link as non-activatable and indicate that the target is unavailable.
4. IF a Polygon containing a Cross_Refraction link is rendered and the target Polygon resides in a Sphere the viewer cannot access, THEN THE Rich_Renderer SHALL display the link as non-activatable, withhold the target Polygon's name, and indicate that the target is inaccessible.

### Requirement 15: ID-Based Linking and Identifiers

**User Story:** As a User, I want links referenced by identifier, so that renaming a Polygon does not break links.

#### Acceptance Criteria

1. THE Influence_System SHALL reference Polygons in links by Record_ID rather than by display name.
2. THE Influence_System SHALL construct URLs for Spheres, Facets, and Polygons using their Record_IDs.
3. WHEN a Polygon is renamed, THE Influence_System SHALL preserve all existing links to that Polygon such that each link continues to reference the same Record_ID and resolves to the Polygon's updated name.
4. THE Influence_System SHALL assign each Sphere, Facet, and Polygon that appears in the user interface a Record_ID that is a UUID or Short UUID, unique across all records of that type.
5. THE Influence_System SHALL keep each assigned Record_ID unchanged for the lifetime of the Sphere, Facet, or Polygon it identifies.

### Requirement 16: Sensitive-Data Encryption and Obfuscation

**User Story:** As an author, I want to encrypt sensitive text with per-Sphere reveal control, so that only permitted people can see it.

#### Acceptance Criteria

1. WHEN a User selects a span of content and requests encryption, THE Influence_System SHALL encrypt the selected span into ciphertext using a Tenant-scoped key derived from the Tenant_Salt of the User's Tenant, and SHALL store the resulting ciphertext keyed by a unique id.
2. WHEN a span is encrypted, THE Influence_System SHALL replace the span in the stored content with an Obfuscation_Token of the form `|encrypt|{id}|`, where {id} maps to the stored ciphertext, and SHALL retain no plaintext copy of the encrypted span in the stored content.
3. WHEN a Polygon containing an Obfuscation_Token is rendered for a User without Reveal_Permission for that Sphere, THE Rich_Renderer SHALL replace the token with a masked placeholder and SHALL never include the corresponding Sensitive_Data plaintext in the rendered output.
4. WHERE a User holds Reveal_Permission for a Sphere, WHEN that User requests to reveal an Obfuscation_Token within that Sphere, THE Influence_System SHALL decrypt and return the Sensitive_Data for that single token, and SHALL keep all other Obfuscation_Tokens masked until each is individually requested.
5. IF a User without Reveal_Permission for a Sphere requests to reveal an Obfuscation_Token in that Sphere, THEN THE Influence_System SHALL deny the reveal, keep the content masked, and return a response indicating the reveal was not permitted.
6. THE Influence_System SHALL grant Reveal_Permission on a per-Sphere basis, such that Reveal_Permission for one Sphere does not permit revealing Obfuscation_Tokens in any other Sphere.
7. WHEN content containing an Obfuscation_Token is exported to PDF or indexed for search for a context lacking Reveal_Permission for that Sphere, THE Influence_System SHALL include only the masked placeholder and SHALL exclude the corresponding Sensitive_Data plaintext and ciphertext from the export and the search index.
8. IF a reveal request references an Obfuscation_Token whose stored ciphertext is missing or cannot be decrypted, THEN THE Influence_System SHALL keep the content masked and return a response indicating the reveal failed, without exposing partial plaintext.
9. WHEN a Tenant database is exported for portability, THE Influence_System SHALL include or re-establish the Tenant-scoped key material required to decrypt stored ciphertext on the successor host, such that Obfuscation_Tokens remain decryptable for Users with Reveal_Permission after import.

### Requirement 17: Image Paste

**User Story:** As a technical author, I want to paste images directly, so that I can add screenshots without a separate upload step.

#### Acceptance Criteria

1. WHEN a User pastes an image in PNG, JPEG, GIF, or WebP format not exceeding 10 MB into the Markdown_Editor, THE Influence_System SHALL store the image as a binary blob within the User's Tenant_Database and insert a reference to the image in the Polygon content.
2. IF a User pastes an image that is not in PNG, JPEG, GIF, or WebP format, THEN THE Influence_System SHALL reject the paste, leave the Polygon content unchanged, and display an error message indicating the image format is unsupported.
3. IF a User pastes an image exceeding 10 MB, THEN THE Influence_System SHALL reject the paste, leave the Polygon content unchanged, and display an error message indicating the image exceeds the maximum allowed size.
4. WHEN a Polygon containing an image reference is rendered, THE Rich_Renderer SHALL display the referenced image.
5. IF a Polygon contains an image reference that cannot be resolved within the Tenant_Database, THEN THE Rich_Renderer SHALL display a placeholder indicating the image is unavailable.

### Requirement 18: Page Metadata Footer

**User Story:** As a reader, I want to see authorship and dates, so that I know a page's provenance and freshness.

#### Acceptance Criteria

1. WHEN a Polygon is created, THE Influence_System SHALL record the authoring User's identifier and the creation date as an ISO 8601 timestamp in UTC.
2. WHEN a Polygon is edited, THE Influence_System SHALL record the last-edit date as an ISO 8601 timestamp in UTC.
3. WHEN a Polygon is rendered, THE Rich_Renderer SHALL display a footer containing the authoring User's display name, the creation date, and the last-edit date, with each date formatted as an ISO 8601 date in UTC.

### Requirement 19: SSL Support

**User Story:** As a platform operator, I want SSL, so that traffic between clients and the server is encrypted.

#### Acceptance Criteria

1. WHERE SSL is configured, THE Influence_System SHALL serve client traffic over a TLS connection using TLS version 1.2 or higher.
2. WHERE SSL is configured, IF a client connects using a plaintext (non-TLS) request, THEN THE Influence_System SHALL refuse to serve the request over plaintext.
3. THE Influence_System SHALL accept configuration of an SSL certificate and private key.
4. IF SSL is configured with a certificate or private key that is missing, unreadable, or mismatched, THEN THE Influence_System SHALL fail to start and emit an error message indicating the SSL configuration is invalid.

### Requirement 20: Linux Deployment

**User Story:** As a platform operator, I want to run the server standalone on common Linux distributions, so that hosting is simple and inexpensive.

#### Acceptance Criteria

1. THE Influence_System SHALL run as a standalone server process on Ubuntu Linux LTS releases version 20.04 or later.
2. THE Influence_System SHALL run as a standalone server process on Fedora Linux version 38 or later.
3. THE Influence_System SHALL operate using SQLite database files without requiring a separate database server.
4. IF a configured SQLite database file is missing or inaccessible at startup, THEN THE Influence_System SHALL fail to start and emit an error message indicating the database file cannot be accessed.

### Requirement 21: Left-Menu Navigation

**User Story:** As a User, I want a left menu of Spheres and the selected Sphere's contents, so that I can navigate quickly.

#### Acceptance Criteria

1. THE Influence_System SHALL display a left menu listing only the Spheres the current User has access to, ordered per Requirement 7.
2. WHEN a User selects a Sphere, THE Influence_System SHALL display that Sphere's Facets and Polygons in the left menu.
3. IF a selected Sphere contains no Facets and no Polygons, THEN THE Influence_System SHALL display an empty-contents indicator in the left menu for that Sphere.

### Requirement 22: PDF Export

**User Story:** As a User, I want to export a page to PDF, so that I can share content with people who have no account.

#### Acceptance Criteria

1. WHEN a User requests a PDF_Export of a Polygon, THE Influence_System SHALL generate a PDF containing the rendered Polygon content and exclude all toolbars and menus from the generated PDF.
2. WHERE the requesting User lacks Reveal_Permission for sensitive data within the Polygon, THE Influence_System SHALL render that sensitive data in masked form in the generated PDF.
3. IF PDF_Export generation fails, THEN THE Influence_System SHALL abort the export, produce no PDF file, and return an error indication to the requesting User.

### Requirement 23: Search

**User Story:** As a User, I want to search content, so that I can find Polygons by their related content.

#### Acceptance Criteria

1. WHEN a User submits a search query, THE Search_Service SHALL return only Polygons whose content matches the query and that reside in Spheres the User has access to.
2. WHERE the requesting User lacks Reveal_Permission for sensitive data, THE Search_Service SHALL exclude that sensitive plaintext from matching and from returned results.
3. IF no Polygon matches a search query, THEN THE Search_Service SHALL return an empty result set.

### Requirement 24: Quick Searches

**User Story:** As a User, I want quick-access lists, so that I can find my recent and authored content fast.

#### Acceptance Criteria

1. WHEN a User requests "Polygons I've Created", THE Search_Service SHALL return the Polygons authored by that User that reside in Spheres the User has access to, ordered from most recently created to least recently created.
2. WHEN a User requests "Recently Viewed Polygons", THE Search_Service SHALL return up to the 50 Polygons that User has most recently viewed within Spheres the User has access to, ordered from most recent to least recent.
3. IF a requested quick-search list contains no matching Polygons, THEN THE Search_Service SHALL return an empty result set.

### Requirement 25: Concurrent Editing Protection

**User Story:** As an author, I want protection against simultaneous edits, so that concurrent edits do not lose data.

#### Acceptance Criteria

1. THE Influence_System SHALL operate each Polygon in exactly one of two mutually exclusive edit modes: collaborative editing or record locking.
2. WHERE collaborative editing is enabled for a Polygon, WHEN a User edits that Polygon, THE Influence_System SHALL propagate that User's changes to every other User editing the same Polygon within 5 seconds.
3. WHERE record locking is enabled for a Polygon, WHEN a User begins editing that Polygon, THE Influence_System SHALL acquire an edit lock for that User and prevent other Users from saving edits to that Polygon until the lock is released.
4. WHERE record locking is enabled, IF a second User attempts to edit a locked Polygon, THEN THE Influence_System SHALL reject the edit and notify that User that the Polygon is currently locked and by whom.
5. WHERE record locking is enabled, THE Influence_System SHALL release an edit lock upon explicit release by the lock holder, after 300 seconds of lock-holder inactivity, or upon lock-holder disconnection.

### Requirement 26: Bookmarks

**User Story:** As a User, I want to bookmark Polygons and see them grouped by Sphere, so that I can return to important content.

#### Acceptance Criteria

1. WHEN a User bookmarks a Polygon, THE Influence_System SHALL add that Polygon to that User's Bookmark list.
2. IF a User bookmarks a Polygon already in that User's Bookmark list, THEN THE Influence_System SHALL leave that User's Bookmark list unchanged.
3. WHEN a User opens the bookmarks menu item, THE Influence_System SHALL list that User's bookmarked Polygons grouped by Sphere.
4. IF a User's Bookmark list is empty, THEN THE Influence_System SHALL display an empty-bookmarks indicator.
5. WHEN a User removes a Bookmark, THE Influence_System SHALL remove that Polygon from that User's Bookmark list.

### Requirement 27: Documentation Generation

**User Story:** As a maintainer, I want documentation authored in markdown, so that it can be published on GitHub and inside the app.

#### Acceptance Criteria

1. THE Influence_System SHALL maintain platform documentation authored in markdown.
2. WHEN a User navigates to the in-app documentation, THE Influence_System SHALL render the platform documentation markdown within the Influence web application.
3. IF the requested documentation is unavailable, THEN THE Influence_System SHALL display an indication that the documentation could not be loaded.
4. WHERE documentation is published to a repository, THE Influence_System SHALL provide the documentation as unmodified markdown files suitable for GitHub.
### Requirement 28: Build and Deployment Tooling

**User Story:** As a platform operator, I want a Makefile with dev, build, install, and uninstall targets, so that I can build and set up the server as a managed service easily.

#### Acceptance Criteria

1. THE Influence_System SHALL provide a Makefile with `dev`, `build`, `install`, and `uninstall` targets.
2. WHEN an operator runs the `dev` target, THE Makefile SHALL start the Influence_System as a foreground process that listens for connections and exits when the operator interrupts it, without creating a Systemd_Service.
3. WHEN an operator runs the `build` target, THE Makefile SHALL produce a runnable server binary and exit with a success status; IF the build fails, THEN THE Makefile SHALL exit with a non-success status and emit an error indicating the build failure.
4. WHEN an operator runs the `install` target, THE Makefile SHALL install the server binary, create a dedicated non-login service user under which the Influence_System runs, install a default configuration file if none already exists, and create a Systemd_Service that runs the Influence_System as that service user.
5. WHEN an operator runs the `uninstall` target, THE Makefile SHALL stop the Systemd_Service if running, remove the Systemd_Service, and remove the installed server binary.
6. WHEN an operator runs the `uninstall` target, THE Makefile SHALL leave the Data_Directory and its database files in place unless the operator explicitly requests their removal.
7. WHILE the Systemd_Service or installed binary already exists, THE Makefile SHALL complete the `install` and `uninstall` targets without error when re-run, producing the same end state as a single run.

### Requirement 29: Configurable Listen Port

**User Story:** As a platform operator, I want to run the server on a non-standard port, so that it can coexist with other services on the same host.

#### Acceptance Criteria

1. THE Influence_System SHALL accept a command-line parameter that specifies the network port, as an integer in the range 1 to 65535 inclusive, on which the server listens.
2. WHERE no listen port parameter is provided, THE Influence_System SHALL listen on the default port 8080.
3. IF the listen port parameter is not an integer in the range 1 to 65535 inclusive, THEN THE Influence_System SHALL fail to start, make no attempt to bind a port, and emit an error indicating the port value is invalid.
4. IF the specified listen port is a valid port number but is already in use, THEN THE Influence_System SHALL fail to start and emit an error indicating the port is unavailable.

### Requirement 30: Configurable Data Directory

**User Story:** As a platform operator, I want to specify where databases are stored, so that I can control data placement and backups.

#### Acceptance Criteria

1. THE Influence_System SHALL accept a configuration parameter that specifies the Data_Directory in which the Central_Directory and Tenant_Database files are stored.
2. WHERE no Data_Directory parameter is provided, THE Influence_System SHALL use the default Data_Directory /var/lib/influence.
3. WHEN the Influence_System starts, THE Influence_System SHALL store and read the Central_Directory and all Tenant_Database files within the configured Data_Directory.
4. IF the configured Data_Directory does not exist at startup, THEN THE Influence_System SHALL fail to start and emit an error indicating the Data_Directory does not exist.
5. IF the configured Data_Directory exists but is not writable by the running process at startup, THEN THE Influence_System SHALL fail to start and emit an error indicating the Data_Directory is not writable.

### Requirement 31: First-Run Administrator Bootstrap

**User Story:** As a new operator who has just installed the platform, I want to create the first administrator from the browser, so that I can begin using the system without hand-editing the database.

#### Acceptance Criteria

1. WHILE a Tenant is in First_Run_State, WHEN a User loads the web application for that Tenant, THE Influence_System SHALL display the Setup_Screen in place of the login screen.
2. WHILE a Tenant is in First_Run_State, WHEN a User submits a Setup_Screen request with a username of 1 to 100 characters and a password meeting the platform password policy, THE Influence_System SHALL create a Bootstrap_Admin User in the Central_Directory assigned to that Tenant, store the password as a one-way Argon2id hash, and add the Bootstrap_Admin to that Tenant's Admin_Group.
3. WHEN the Bootstrap_Admin is created, THE Influence_System SHALL leave the Tenant no longer in First_Run_State and SHALL thereafter display the normal login screen for that Tenant.
4. IF a Setup_Screen request is submitted while the Tenant is not in First_Run_State, THEN THE Influence_System SHALL reject the request, create no User, and return an error indicating setup has already been completed.
5. IF a Setup_Screen request supplies an empty username, a username exceeding 100 characters, or a password that does not meet the platform password policy, THEN THE Influence_System SHALL reject the request, create no User, and return a validation error.
6. WHILE a Tenant is not in First_Run_State, THE Influence_System SHALL deny access to the first-run setup endpoint for that Tenant.

### Requirement 32: Administrative and Identity API Surface

**User Story:** As an Admin_Group member using the administration pages, I want the backend endpoints that those pages depend on, so that user, group, and access management actually take effect.

#### Acceptance Criteria

1. WHEN an authenticated User requests the Identity_Endpoint, THE Influence_System SHALL return that User's identifier, display name, Admin_Group membership status, and Two_Factor_Authentication enrolment status for the current session.
2. WHERE a User is a member of the Admin_Group, THE Admin_API SHALL provide operations to list Users within the caller's Tenant, create a User, deactivate a User, and reactivate a User.
3. WHERE a User is a member of the Admin_Group, THE Admin_API SHALL provide operations to list Groups within the caller's Tenant, create a Group, add a User to a Group, and remove a User from a Group, subject to the at-least-one-Group invariant of Requirement 4.1.
4. WHERE a User is a member of the Admin_Group, THE Admin_API SHALL provide operations to list per-Sphere Group grants within the caller's Tenant and to grant or revoke a Group's access to a Sphere, including access level and Reveal_Permission, consistent with Requirement 4.
5. IF a User who is not a member of the Admin_Group requests any Admin_API operation, THEN THE Influence_System SHALL deny the request and return an error indicating administrative privileges are required.
6. THE Influence_System SHALL restrict every Admin_API operation to Users, Groups, and Spheres within the requesting User's authenticated Tenant, and SHALL reject any Admin_API request that references an entity outside that Tenant per Requirement 1.6.
7. WHEN an Admin_API create or mutation operation fails, THE Influence_System SHALL leave the affected Users, Groups, and grants unchanged and return the standard error envelope with an appropriate error code.

### Requirement 33: Two-Factor Authentication

**User Story:** As a security-conscious User, I want to protect my account with an authenticator app, and as an administrator I want to be able to require it, so that accounts are harder to compromise.

#### Acceptance Criteria

1. WHEN a User initiates Two_Factor_Authentication enrolment, THE Influence_System SHALL generate a TOTP_Secret for that User, store it in the Central_Directory, and return a TOTP_Provisioning_URI that the web application renders as a scannable QR code together with the secret in text form.
2. WHEN a User submits a valid TOTP that matches the pending TOTP_Secret during enrolment, THE Influence_System SHALL mark Two_Factor_Authentication as enrolled for that User and issue a set of single-use Recovery_Codes.
3. IF a User submits an invalid TOTP during enrolment, THEN THE Influence_System SHALL not mark Two_Factor_Authentication as enrolled and SHALL return an error indicating the code was invalid.
4. WHERE a User has Two_Factor_Authentication enrolled, WHEN that User submits a valid password, THE Influence_System SHALL require a valid TOTP or a single-use Recovery_Code before establishing an authenticated session.
5. IF a User with Two_Factor_Authentication enrolled submits a valid password but an invalid TOTP and an invalid Recovery_Code, THEN THE Influence_System SHALL deny access, establish no session, and return an authentication error, applying the brute-force lockout of Requirement 3.3 to repeated second-factor failures.
6. WHEN a User presents a valid Recovery_Code, THE Influence_System SHALL accept it for that single authentication and invalidate that Recovery_Code so it cannot be reused.
7. WHERE a Tenant's Two_Factor_Policy is required, IF a User of that Tenant who has not enrolled Two_Factor_Authentication authenticates with a valid password, THEN THE Influence_System SHALL establish a restricted session that permits only Two_Factor_Authentication enrolment and logout until enrolment is completed.
8. WHERE a User is a member of the Admin_Group, THE Influence_System SHALL provide an operation to set the Two_Factor_Policy for the caller's Tenant to either optional or required.
9. WHEN a TOTP is validated, THE Influence_System SHALL accept a code from the current time step and immediately adjacent time steps to tolerate clock drift, and SHALL reject a TOTP that has already been consumed within its validity window.

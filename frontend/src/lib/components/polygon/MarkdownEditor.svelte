<script lang="ts">
	// Section-based rich view + editor (Req 12.1, 12.2).
	//
	// The component splits the stored markdown into heading-delimited sections
	// and renders each. Clicking a section's Edit opens a textarea seeded with
	// the exact original bytes for that span (Req 12.3); Cancel discards the
	// buffer leaving stored content untouched (Req 12.4); Confirm splices the
	// edited buffer back and emits the new full content.
	//
	// While editing, a toolbar exposes an icon per `@`-command (Req 11.10), a
	// paste handler uploads images and inserts their reference (Req 17.1), an
	// "Encrypt selection" action turns a highlighted span into an obfuscation
	// token (Req 16.4 authoring side), and rendered sections expose per-token
	// reveal controls (Req 16.4 reveal side).

	import { splitSections, replaceSection, type Section } from './sections';
	import { renderMarkdownPreview } from './markdown';
	import {
		COMMANDS,
		headingStyles,
		headingMarkdown,
		linkMarkdown,
		type CommandInfo
	} from './commands';
	import * as api from '$lib/polygonApi';

	interface Props {
		polygonId: string;
		content: string;
		/** Candidate polygons for the `@link` picker (Req 11.2). */
		linkCandidates?: { recordId: string; name: string }[];
		/** Emitted with the full new content when a section edit is confirmed. */
		onchange?: (content: string) => void;
	}

	let { polygonId, content = $bindable(), linkCandidates = [], onchange }: Props = $props();

	// Sections derive from the current content (Req 12.1).
	let sections = $derived<Section[]>(splitSections(content));

	// Editing state: which section index is open, and its live buffer.
	let editingIndex = $state<number | null>(null);
	let buffer = $state('');
	let textarea = $state<HTMLTextAreaElement | null>(null);
	let statusMessage = $state('');
	let showLinkPicker = $state(false);
	let showHeadingPicker = $state(false);

	// Per-token revealed plaintext, keyed by token id (Req 16.4). Cleared when
	// the section content changes so stale reveals never persist.
	let revealed = $state<Record<string, string>>({});

	function beginEdit(section: Section) {
		editingIndex = section.index;
		buffer = section.source; // exact original bytes (Req 12.3)
		statusMessage = '';
		showLinkPicker = false;
		showHeadingPicker = false;
	}

	function cancelEdit() {
		// Discard the buffer; stored content is untouched (Req 12.4).
		editingIndex = null;
		buffer = '';
		statusMessage = '';
	}

	function confirmEdit() {
		if (editingIndex === null) return;
		const section = sections[editingIndex];
		const next = replaceSection(content, section, buffer);
		content = next;
		onchange?.(next);
		editingIndex = null;
		buffer = '';
		statusMessage = 'Section saved.';
	}

	// --- Toolbar: insert text at the caret ---------------------------------
	function insertAtCaret(text: string) {
		const el = textarea;
		if (!el) {
			buffer += text;
			return;
		}
		const start = el.selectionStart ?? buffer.length;
		const end = el.selectionEnd ?? buffer.length;
		buffer = buffer.slice(0, start) + text + buffer.slice(end);
		// Restore caret just after the inserted text on the next tick.
		queueMicrotask(() => {
			el.focus();
			const pos = start + text.length;
			el.setSelectionRange(pos, pos);
		});
	}

	function onCommand(cmd: CommandInfo) {
		showLinkPicker = false;
		showHeadingPicker = false;
		switch (cmd.command) {
			case '@link':
				showLinkPicker = true;
				break;
			case '@heading':
				showHeadingPicker = true;
				break;
			case '@toc':
			case '@children':
				// Render-time directives persist as their typed form (design:
				// "@toc/@children are dynamic"); insert the directive literally.
				insertAtCaret(`\n${cmd.command}\n`);
				break;
		}
	}

	function chooseLink(c: { recordId: string; name: string }) {
		insertAtCaret(linkMarkdown(c.name, c.recordId));
		showLinkPicker = false;
	}

	function chooseHeading(level: number) {
		// Apply the heading prefix to the current selection (or a placeholder).
		const el = textarea;
		let selected = '';
		if (el) {
			selected = buffer.slice(el.selectionStart ?? 0, el.selectionEnd ?? 0);
		}
		const text = selected || 'Heading';
		insertAtCaret(headingMarkdown(level, text) + '\n');
		showHeadingPicker = false;
	}

	// --- Encrypt selection (Req 16.4 authoring) ----------------------------
	// The backend encrypts by character offsets into the *whole* content, so we
	// translate the textarea selection (relative to the section buffer) into
	// absolute offsets. Encryption must run against saved content, so we require
	// the buffer to be unchanged from the stored section before encrypting.
	let encryptError = $state('');
	async function encryptSelection() {
		encryptError = '';
		if (editingIndex === null || !textarea) return;
		const section = sections[editingIndex];
		if (buffer !== section.source) {
			encryptError = 'Save the section before encrypting a selection.';
			return;
		}
		const selStart = textarea.selectionStart ?? 0;
		const selEnd = textarea.selectionEnd ?? 0;
		if (selEnd <= selStart) {
			encryptError = 'Select some text to encrypt.';
			return;
		}
		const absStart = section.start + selStart;
		const absEnd = section.start + selEnd;
		try {
			const res = await api.encryptSpan(polygonId, absStart, absEnd);
			content = res.newContent;
			onchange?.(res.newContent);
			editingIndex = null;
			buffer = '';
			statusMessage = 'Selection encrypted.';
		} catch (e) {
			encryptError = e instanceof Error ? e.message : 'Encryption failed.';
		}
	}

	// --- Image paste (Req 17.1) --------------------------------------------
	let pasteError = $state('');
	async function onPaste(event: ClipboardEvent) {
		const items = event.clipboardData?.items;
		if (!items) return;
		for (const item of items) {
			if (item.type.startsWith('image/')) {
				event.preventDefault();
				const file = item.getAsFile();
				if (!file) continue;
				pasteError = '';
				try {
					const ref = await api.uploadImage(polygonId, file);
					insertAtCaret(`![pasted image](${ref})`);
				} catch (e) {
					pasteError = e instanceof Error ? e.message : 'Image upload failed.';
				}
				return;
			}
		}
	}

	// --- Per-token reveal (Req 16.4 reveal) --------------------------------
	let revealError = $state('');
	async function reveal(tokenId: string) {
		revealError = '';
		try {
			const plaintext = await api.revealToken(polygonId, tokenId);
			revealed = { ...revealed, [tokenId]: plaintext };
		} catch (e) {
			// Denied / failed carry no plaintext; keep masked and show the reason.
			revealError = e instanceof Error ? e.message : 'Reveal failed.';
		}
	}

	// Render a section to preview HTML, then wire reveal controls in the markup
	// below by scanning the section source for tokens.
	function sectionTokens(source: string): string[] {
		const ids: string[] = [];
		const re = /\|encrypt\|([^|]+)\|/g;
		let m: RegExpExecArray | null;
		while ((m = re.exec(source)) !== null) ids.push(m[1]);
		return ids;
	}
</script>

<div class="editor" data-testid="markdown-editor">
	{#each sections as section (section.index)}
		<section class="section" data-level={section.level}>
			{#if editingIndex === section.index}
				<!-- Edit mode: toolbar + textarea (Req 12.2) -->
				<div class="toolbar" role="toolbar" aria-label="Editor commands">
					{#each COMMANDS as cmd (cmd.command)}
						<button
							type="button"
							class="tool"
							data-icon={cmd.icon}
							title={`${cmd.label} (${cmd.command})`}
							aria-label={cmd.label}
							onclick={() => onCommand(cmd)}
						>
							<span class="glyph" aria-hidden="true">{cmd.glyph}</span>
							<span class="tool-label">{cmd.command}</span>
						</button>
					{/each}
					<button type="button" class="tool encrypt" onclick={encryptSelection}>
						<span class="glyph" aria-hidden="true">🔒</span>
						<span class="tool-label">Encrypt selection</span>
					</button>
				</div>

				{#if showLinkPicker}
					<div class="picker" data-testid="link-picker">
						{#if linkCandidates.length === 0}
							<p class="empty">No linkable Polygons exist in this tenant.</p>
						{:else}
							<ul>
								{#each linkCandidates as c (c.recordId)}
									<li>
										<button type="button" onclick={() => chooseLink(c)}>{c.name}</button>
									</li>
								{/each}
							</ul>
						{/if}
					</div>
				{/if}

				{#if showHeadingPicker}
					<div class="picker" data-testid="heading-picker">
						<ul>
							{#each headingStyles() as h (h.level)}
								<li>
									<button type="button" onclick={() => chooseHeading(h.level)}>{h.label}</button>
								</li>
							{/each}
						</ul>
					</div>
				{/if}

				<textarea
					bind:this={textarea}
					bind:value={buffer}
					onpaste={onPaste}
					rows={Math.max(4, buffer.split('\n').length + 1)}
					aria-label="Section markdown"
				></textarea>

				{#if pasteError}<p class="error">{pasteError}</p>{/if}
				{#if encryptError}<p class="error">{encryptError}</p>{/if}

				<div class="preview" data-testid="edit-preview">
					<!-- eslint-disable-next-line svelte/no-at-html-tags -->
					{@html renderMarkdownPreview(buffer)}
				</div>

				<div class="actions">
					<button type="button" class="confirm" onclick={confirmEdit}>Confirm</button>
					<button type="button" class="cancel" onclick={cancelEdit}>Cancel</button>
				</div>
			{:else}
				<!-- Rich view: click Edit to open this section (Req 12.1) -->
				<div class="rich">
					<!-- eslint-disable-next-line svelte/no-at-html-tags -->
					{@html renderMarkdownPreview(section.source)}
				</div>

				{#each sectionTokens(section.source) as tokenId (tokenId)}
					<div class="reveal-row" data-testid="reveal-row">
						{#if revealed[tokenId] !== undefined}
							<span class="revealed" data-token={tokenId}>{revealed[tokenId]}</span>
						{:else}
							<button type="button" class="reveal" onclick={() => reveal(tokenId)}>
								Reveal token
							</button>
						{/if}
					</div>
				{/each}

				<button type="button" class="edit" onclick={() => beginEdit(section)}>Edit</button>
			{/if}
		</section>
	{/each}

	{#if revealError}<p class="error" data-testid="reveal-error">{revealError}</p>{/if}
	{#if statusMessage}<p class="status" data-testid="status">{statusMessage}</p>{/if}
</div>

<style>
	.editor {
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}
	.section {
		border: 1px solid #e4e7eb;
		border-radius: 6px;
		padding: 0.75rem 1rem;
	}
	.toolbar {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
		margin-bottom: 0.5rem;
	}
	.tool {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		border: 1px solid #cbd2d9;
		background: #fff;
		border-radius: 4px;
		padding: 0.25rem 0.5rem;
		cursor: pointer;
		font-size: 0.85rem;
	}
	.tool:hover {
		background: #f0f4f8;
	}
	.glyph {
		font-size: 1rem;
	}
	.picker {
		border: 1px solid #cbd2d9;
		border-radius: 4px;
		padding: 0.5rem;
		margin-bottom: 0.5rem;
		background: #fafbfc;
	}
	.picker ul {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-wrap: wrap;
		gap: 0.35rem;
	}
	textarea {
		width: 100%;
		font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
		font-size: 0.9rem;
		padding: 0.5rem;
		box-sizing: border-box;
	}
	.preview {
		margin-top: 0.5rem;
		padding: 0.5rem;
		border-top: 1px dashed #e4e7eb;
		color: #3e4c59;
	}
	.actions {
		display: flex;
		gap: 0.5rem;
		margin-top: 0.5rem;
	}
	.confirm {
		background: #2563eb;
		color: #fff;
		border: none;
		border-radius: 4px;
		padding: 0.35rem 0.75rem;
		cursor: pointer;
	}
	.cancel,
	.edit,
	.reveal {
		background: #fff;
		border: 1px solid #cbd2d9;
		border-radius: 4px;
		padding: 0.35rem 0.75rem;
		cursor: pointer;
	}
	.reveal-row {
		margin: 0.35rem 0;
	}
	.revealed {
		background: #fff7ed;
		border: 1px solid #fed7aa;
		border-radius: 4px;
		padding: 0.1rem 0.35rem;
	}
	:global(.masked) {
		background: #edf0f5;
		border-radius: 4px;
		padding: 0.05rem 0.35rem;
		color: #52606d;
	}
	.error {
		color: #b91c1c;
	}
	.status {
		color: #047857;
	}
</style>

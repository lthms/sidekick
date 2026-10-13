// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

import { execFile } from "child_process";
import * as fs from "fs";
import * as http from "http";
import * as https from "https";
import * as os from "os";
import * as path from "path";
import { promisify } from "util";
import * as vscode from "vscode";

const execFileAsync = promisify(execFile);

/** Command ids, as contributed in package.json. */
const commands = {
    notify: "sidekick.notify",
    interrupt: "sidekick.interrupt",
    restart: "sidekick.restart",
    changeModel: "sidekick.changeModel",
    setup: "sidekick.setup",
    showQuestion: "sidekick.showQuestion",
} as const;

/** Ctrl-C, the byte a TUI reads as "interrupt". */
const interruptByte = String.fromCharCode(3);

/**
 * Delay left between a prompt and its submission in claude's TUI. Sending both
 * at once races the TUI's autocomplete, which then mangles the prompt.
 */
const promptDelay = 300;

/**
 * Cap on the xref results the daemon renders. One result past it is still
 * collected, which is how the daemon detects that the list was truncated, and
 * only the rendered ones have their target line read: a provider answering
 * thousands of references would otherwise cost a document load each.
 */
const maxLocations = 200;

const severityNames: Record<vscode.DiagnosticSeverity, string> = {
    [vscode.DiagnosticSeverity.Error]: "error",
    [vscode.DiagnosticSeverity.Warning]: "warning",
    [vscode.DiagnosticSeverity.Information]: "information",
    [vscode.DiagnosticSeverity.Hint]: "hint",
};

/** Body of an operation the daemon sends (see the vscodeRequest type of vscode.go). */
interface OpRequest {
    op?: string;
    buf?: string;
    path?: string;
    start?: number;
    end?: number;
    previous?: string[];
    content?: string[];
    line?: number;
    col?: number;
    prompt?: string;
    choices?: string[];
}

/** One definition or use site, in the shape the daemon decodes. */
interface XrefLocation {
    file: string;
    line: number;
    summary: string;
}

/** One problem a language server reported, in the shape the daemon decodes. */
interface ReportedDiagnostic {
    line: number;
    column: number;
    severity: string;
    message: string;
}

/** The settings a session reads, resolved once per use. */
interface Config {
    serverUrl: string;
    defaultModel: string;
    autoInstall: boolean;
    /** Marketplace to install the plugin from, either a local path or "repo#ref". */
    marketplace: string;
}

/**
 * What a registered session is made of. All of it outlives the claude process,
 * which is why a restart does not register again.
 */
interface Session {
    /** Session key, the extension host's pid: one per VSCode window. */
    pid: number;
    /** Session root, the first workspace folder; buffer ids are relative to it. */
    root: vscode.Uri;
    serverUrl: string;
    /** Path of the generated mcp config claude is spawned with. */
    mcpConfig: string;
}

let session: Session | undefined;
let opServer: http.Server | undefined;
/** Endpoint of the operation server, as registered with the daemon. */
let opEndpoint: string | undefined;
let claudeTerminal: vscode.Terminal | undefined;
let statusBar: vscode.StatusBarItem | undefined;
let nextRequestId = 0;

/**
 * Absolute paths of the documents open_buffer loaded. They have no editor tab
 * on purpose, so they are the half of the tools' perimeter that window.tabGroups
 * cannot report.
 */
const openedByClaude = new Set<string>();

/**
 * Uris a diagnostics provider has published for, as uri strings. Membership is
 * the proof an analysis happened; see documentDiagnostics.
 */
const analyzedDocuments = new Set<string>();

/** The question a quick pick is currently asking, if any. */
let pendingQuestion: { question: string; show: () => void } | undefined;

/** Tail of the chain that serializes ask operations. */
let askQueue: Promise<unknown> = Promise.resolve();

/** Tail of the chain that serializes edit operations. */
let editQueue: Promise<unknown> = Promise.resolve();

/**
 * The session start in flight, if any. Startups are triggered from three
 * places (activation, a folder being added, the Setup command) and registering
 * twice would spawn a second claude that nothing can reach afterwards.
 */
let startup: Promise<void> | undefined;

export function activate(context: vscode.ExtensionContext): void {
    statusBar = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 0);
    context.subscriptions.push(statusBar);
    refreshStatusBar();

    context.subscriptions.push(
        vscode.commands.registerCommand(commands.notify, notify),
        vscode.commands.registerCommand(commands.interrupt, interrupt),
        vscode.commands.registerCommand(commands.restart, restart),
        vscode.commands.registerCommand(commands.changeModel, changeModel),
        vscode.commands.registerCommand(commands.setup, setup),
        vscode.commands.registerCommand(commands.showQuestion, showQuestion),
        vscode.languages.onDidChangeDiagnostics((event) => {
            for (const uri of event.uris) {
                analyzedDocuments.add(uri.toString());
            }
        }),
        vscode.window.onDidCloseTerminal((terminal) => {
            if (terminal === claudeTerminal) {
                claudeTerminal = undefined;
                refreshStatusBar();
            }
        }),
        vscode.workspace.onDidChangeWorkspaceFolders(() => {
            // A window that activates empty has no root to register, so the
            // session starts when it gets its first folder instead.
            if (session === undefined && vscode.workspace.workspaceFolders?.length) {
                void startSessionOnce();
            }
        }),
    );

    void startSessionOnce();
}

export function deactivate(): void {
    opServer?.close();
    opServer = undefined;
}

function readConfig(): Config {
    const settings = vscode.workspace.getConfiguration("sidekick");
    const local = settings.get<string | null>("claude.marketplace.path") ?? null;
    const repo = settings.get<string>("claude.marketplace.repo", "lthms/sidekick");
    const ref = settings.get<string>("claude.marketplace.ref", "main");

    return {
        // The daemon's routes are appended to this, so a trailing slash would
        // turn /mcp/<pid> into //mcp/<pid>.
        serverUrl: settings.get<string>("serverUrl", "http://127.0.0.1:8000").replace(/\/+$/, ""),
        defaultModel: settings.get<string>("claude.defaultModel", "opus"),
        autoInstall: settings.get<boolean>("claude.autoInstall", true),
        marketplace: local ?? `${repo}#${ref}`,
    };
}

// Session lifecycle.

/**
 * Starts a session unless one is already starting, and resolves when that
 * start is over. Callers that may fire while another start is pending go
 * through here rather than calling startSession directly.
 */
function startSessionOnce(): Promise<void> {
    if (startup === undefined) {
        startup = startSession().finally(() => {
            startup = undefined;
        });
    }
    return startup;
}

/**
 * Registers this window with the daemon and starts its claude session.
 *
 * Every step that can fail reports an actionable error and gives up rather
 * than moving on: a claude session that is spawned without a registered
 * endpoint, or without the plugin holding /vscode:monitor, is alive but
 * unreachable, and nothing in its TUI says why. The one silent case is a
 * window with no folder, which is not a failure: the folder listener starts
 * the session if one is ever opened, and the status bar says meanwhile that
 * this window has no session.
 */
async function startSession(): Promise<void> {
    const folder = vscode.workspace.workspaceFolders?.[0];
    if (folder === undefined) {
        return;
    }

    const config = readConfig();
    if (opEndpoint === undefined) {
        try {
            opEndpoint = await startOpServer();
        } catch (err) {
            vscode.window.showErrorMessage(`sidekick cannot serve editor operations: ${describe(err)}`);
            return;
        }
    }

    const pid = process.pid;
    session = {
        pid,
        root: folder.uri,
        serverUrl: config.serverUrl,
        mcpConfig: path.join(os.tmpdir(), `sidekick-vscode-${pid}.json`),
    };

    try {
        await rpcRequest(config.serverUrl, "register", {
            pid,
            app: "vscode",
            endpoint: opEndpoint,
            root: folder.uri.fsPath,
        });
    } catch (err) {
        session = undefined;
        refreshStatusBar();
        vscode.window.showErrorMessage(
            `sidekick daemon unreachable at ${config.serverUrl}, start it then run Sidekick: Setup (${describe(err)})`,
        );
        return;
    }

    // Point this session's claude at the daemon's per-pid MCP endpoint.
    const mcpConfig = { mcpServers: { sidekick: { type: "http", url: `${config.serverUrl}/mcp/${pid}` } } };
    fs.writeFileSync(session.mcpConfig, JSON.stringify(mcpConfig));

    if (!(await ensurePlugin(config))) {
        refreshStatusBar();
        return;
    }

    spawnClaude(config);
}

/**
 * Makes sure claude can resolve /vscode:monitor, installing the plugin when it
 * is missing and installation is allowed. Answers false when the plugin cannot
 * be vouched for, in which case no session must be spawned.
 */
async function ensurePlugin(config: Config): Promise<boolean> {
    const listed = await claude(["plugin", "list", "--json"]).catch(() => "");
    if (listed.includes('"vscode@sidekick"')) {
        return true;
    }
    if (!config.autoInstall) {
        vscode.window.showErrorMessage(
            "sidekick: vscode@sidekick is not installed. Install it, or set sidekick.claude.autoInstall.",
        );
        return false;
    }

    try {
        // Adding a marketplace that is already known fails, which is not a
        // problem here: only the install has to succeed.
        await claude(["plugin", "marketplace", "add", config.marketplace]).catch(() => "");
        await claude(["plugin", "install", "vscode@sidekick"]);
        return true;
    } catch (err) {
        vscode.window.showErrorMessage(`sidekick: cannot install vscode@sidekick: ${describe(err)}`);
        return false;
    }
}

/** Runs the claude CLI and resolves with its standard output. */
async function claude(args: string[]): Promise<string> {
    const { stdout } = await execFileAsync("claude", args, { encoding: "utf8" });
    return stdout;
}

/**
 * Starts claude in an integrated terminal.
 *
 * claude is the terminal's own process rather than a command typed into a
 * shell, which spares the arguments any quoting. The terminal is created but
 * never shown: the session starts without taking the focus, and the user can
 * open the Terminal panel to watch it.
 */
function spawnClaude(config: Config): void {
    const active = requireSession();

    claudeTerminal = vscode.window.createTerminal({
        name: "sidekick claude",
        cwd: active.root,
        shellPath: "claude",
        shellArgs: [
            "--mcp-config",
            active.mcpConfig,
            "--allowedTools",
            "mcp__sidekick",
            "--model",
            config.defaultModel,
            "--",
            `/vscode:monitor ${active.serverUrl} ${active.pid}`,
        ],
    });
    refreshStatusBar();
}

// Commands.

async function notify(): Promise<void> {
    const active = session;
    if (active === undefined) {
        vscode.window.showErrorMessage("sidekick: no session in this window, run Sidekick: Setup");
        return;
    }

    const editor = vscode.window.activeTextEditor;
    if (editor === undefined) {
        vscode.window.showErrorMessage("sidekick: no active editor to notify about");
        return;
    }

    // Buffer ids are file paths, so an untitled document or a notebook cell has
    // nothing claude could open: its fsPath would name a file that does not
    // exist, or another file entirely.
    const uri = editor.document.uri;
    if (uri.scheme !== "file") {
        vscode.window.showErrorMessage(
            `sidekick: no file behind this "${uri.scheme}" editor, save it to disk before notifying claude`,
        );
        return;
    }

    const file = uri.fsPath;
    try {
        await rpcRequest(active.serverUrl, "notify", {
            pid: active.pid,
            buf: bufferId(active.root, file),
            file,
        });
    } catch (err) {
        vscode.window.showErrorMessage(`sidekick: notify failed: ${describe(err)}`);
        return;
    }

    vscode.window.setStatusBarMessage("sidekick: notification sent to claude", 3000);
}

function interrupt(): void {
    const terminal = claudeTerminal;
    if (terminal === undefined) {
        vscode.window.showErrorMessage("sidekick: no claude session to interrupt");
        return;
    }

    // sendText is the only way into a terminal's input the stable extension API
    // offers, so the interrupt travels as its control character (ETX). The
    // newline sendText appends by default is suppressed: claude's TUI would read
    // it as an empty prompt submitted right after the interrupt.
    terminal.sendText(interruptByte, false);
}

function restart(): void {
    if (session === undefined) {
        vscode.window.showErrorMessage("sidekick: no session in this window, run Sidekick: Setup");
        return;
    }

    // The pid, the registry entry and this window's endpoint all outlive
    // claude, so only the process is recreated; registering again would just
    // overwrite the entry with itself.
    claudeTerminal?.dispose();
    claudeTerminal = undefined;
    spawnClaude(readConfig());
}

async function changeModel(): Promise<void> {
    const terminal = claudeTerminal;
    if (terminal === undefined) {
        vscode.window.showErrorMessage("sidekick: no claude session to change the model of");
        return;
    }

    const config = readConfig();
    const model = await vscode.window.showInputBox({
        title: "Sidekick",
        prompt: "model to switch the claude session to",
        value: config.defaultModel,
    });
    if (model === undefined || model === "") {
        return;
    }

    terminal.sendText(`/model ${model}`, false);
    await delay(promptDelay);
    terminal.sendText("\r", false);
    // A second submission accepts the "Switch model" modal the TUI then opens.
    await delay(promptDelay);
    terminal.sendText("\r", false);
}

async function setup(): Promise<void> {
    if (startup !== undefined) {
        // Activation starts a session without waiting for it, so Setup can be
        // run while that one is still registering or installing the plugin.
        vscode.window.showInformationMessage("sidekick: a session is already starting");
        await startup;
        return;
    }
    if (claudeTerminal !== undefined) {
        vscode.window.showInformationMessage(
            "sidekick: a claude session is already running, use Sidekick: Restart to replace it",
        );
        return;
    }
    if (vscode.workspace.workspaceFolders?.[0] === undefined) {
        vscode.window.showErrorMessage(
            "sidekick needs an open folder: the session root is the window's first workspace folder",
        );
        return;
    }

    await startSessionOnce();
}

function showQuestion(): void {
    if (pendingQuestion === undefined) {
        vscode.window.showInformationMessage("sidekick: no question pending");
        return;
    }
    pendingQuestion.show();
}

function refreshStatusBar(): void {
    const item = statusBar;
    if (item === undefined) {
        return;
    }

    if (pendingQuestion !== undefined) {
        item.text = "$(question) sidekick";
        item.tooltip = pendingQuestion.question;
        item.command = commands.showQuestion;
    } else if (claudeTerminal !== undefined) {
        item.text = "$(check) sidekick";
        item.tooltip = "claude is running";
        item.command = undefined;
    } else if (session !== undefined) {
        item.text = "$(debug-disconnect) sidekick";
        item.tooltip = "claude is not running";
        item.command = commands.restart;
    } else {
        item.text = "$(circle-slash) sidekick";
        item.tooltip = "no sidekick session in this window";
        item.command = commands.setup;
    }
    item.show();
}

// Operation server.

/**
 * Starts the operation server on a loopback ephemeral port and resolves with
 * the endpoint to register. Loopback only: the same exposure as the nvim
 * plugin's serverstart("127.0.0.1:0").
 */
function startOpServer(): Promise<string> {
    return new Promise((resolve, reject) => {
        const server = http.createServer((req, res) => {
            void serveOp(req, res);
        });
        server.on("error", reject);
        server.listen(0, "127.0.0.1", () => {
            const address = server.address();
            if (address === null || typeof address === "string") {
                reject(new Error("operation server did not bind a port"));
                return;
            }
            opServer = server;
            resolve(`http://127.0.0.1:${address.port}`);
        });
    });
}

async function serveOp(req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
    let answer: unknown;
    try {
        answer = await dispatch(JSON.parse(await readBody(req)) as OpRequest);
    } catch (err) {
        // Every failure comes back as {"error": …}: the daemon turns it into a
        // tool error, which is what claude can act on.
        answer = { error: describe(err) };
    }

    const body = JSON.stringify(answer);
    res.writeHead(200, { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(body) });
    res.end(body);
}

function readBody(req: http.IncomingMessage): Promise<string> {
    return new Promise((resolve, reject) => {
        const chunks: Buffer[] = [];
        req.on("data", (chunk: Buffer) => chunks.push(chunk));
        req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
        req.on("error", reject);
    });
}

async function dispatch(req: OpRequest): Promise<unknown> {
    switch (req.op) {
        case "read-lines":
            return readLines(req);
        case "edit":
            return editLines(req);
        case "list-buffers":
            return { buffers: openBufferPaths().map((file) => ({ path: file })) };
        case "open":
            return openBuffer(req);
        case "save":
            return saveBuffer(req);
        case "jump":
            return jumpTo(req);
        case "find-definition":
            return xref(req, "vscode.executeDefinitionProvider");
        case "find-references":
            return xref(req, "vscode.executeReferenceProvider");
        case "diagnostics":
            return documentDiagnostics(req);
        case "ask":
            return askUser(req);
        default:
            throw new Error(`unknown op: ${req.op ?? "(none)"}`);
    }
}

// Operations.

async function readLines(req: OpRequest): Promise<{ lines: string[] }> {
    const doc = await document(req.buf);
    const lines = documentLines(doc);

    const start = clamp(req.start ?? 0, 0, lines.length);
    const end = req.end === undefined || req.end < 0 ? lines.length : clamp(req.end, start, lines.length);

    return { lines: lines.slice(start, end) };
}

/**
 * Replaces the lines the daemon last read with new ones, unless they changed
 * since. A stale range comes back as {ok: false} with the line it diverges at,
 * which claude answers by reading again.
 *
 * Edits are serialized, so two requests can never interleave their comparison
 * and their application. What remains outside our reach is a human typing
 * during applyEdit itself: it is asynchronous and takes no version
 * precondition, and the stable API offers no transactional edit for a document
 * no editor shows.
 */
function editLines(req: OpRequest): Promise<{ ok: boolean; reason?: string }> {
    const applied = editQueue.then(
        () => applyLineEdit(req),
        () => applyLineEdit(req),
    );
    editQueue = applied.catch(() => undefined);

    return applied;
}

async function applyLineEdit(req: OpRequest): Promise<{ ok: boolean; reason?: string }> {
    const doc = await document(req.buf);
    const previous = req.previous ?? [];
    const content = req.content ?? [];
    const start = Math.max(req.start ?? 0, 0);
    const stop = start + previous.length;

    const version = doc.version;
    const live = documentLines(doc).slice(start, stop);
    if (live.length !== previous.length) {
        return { ok: false, reason: `range out of date: expected ${previous.length} lines, found ${live.length}` };
    }
    for (let i = 0; i < previous.length; i++) {
        if (live[i] !== previous[i]) {
            return { ok: false, reason: `content changed at line ${start + i + 1}` };
        }
    }

    const { range, text } = lineEdit(doc, start, stop, content);
    const change = new vscode.WorkspaceEdit();
    change.replace(doc.uri, range, text);
    // Last point where the comparison above can still be vouched for.
    if (doc.version !== version) {
        return { ok: false, reason: "buffer changed while the edit was being prepared" };
    }
    if (!(await vscode.workspace.applyEdit(change))) {
        return { ok: false, reason: "vscode refused to apply the edit" };
    }

    return { ok: true };
}

async function openBuffer(req: OpRequest): Promise<{ path: string }> {
    if (req.path === undefined || req.path === "") {
        throw new Error("no path given");
    }

    // openTextDocument loads the file without opening a tab or moving the
    // focus, the counterpart of nvim's bufadd + bufload.
    const doc = await vscode.workspace.openTextDocument(vscode.Uri.file(req.path));
    openedByClaude.add(doc.uri.fsPath);

    return { path: doc.uri.fsPath };
}

async function saveBuffer(req: OpRequest): Promise<Record<string, never>> {
    const doc = await document(req.buf);
    // save() answers false both for a failed write and for a document that had
    // nothing to write, so the second case is settled before asking.
    if (doc.isDirty && !(await doc.save())) {
        throw new Error(`could not save ${doc.uri.fsPath}`);
    }

    return {};
}

async function jumpTo(req: OpRequest): Promise<Record<string, never>> {
    const doc = await document(req.buf);
    const position = positionAt(doc, req.line ?? 1, req.col ?? 1);

    // The one operation that moves the user, by contract. showTextDocument
    // feeds VSCode's navigation history, so Go Back (Alt+Left) returns without
    // any stack of ours.
    await vscode.window.showTextDocument(doc, { selection: new vscode.Range(position, position) });

    return {};
}

async function xref(req: OpRequest, command: string): Promise<{ locations: XrefLocation[] }> {
    const doc = await document(req.buf);
    const position = positionAt(doc, req.line ?? 1, req.col ?? 1);
    const results = await vscode.commands.executeCommand<
        (vscode.Location | vscode.LocationLink)[] | undefined
    >(command, doc.uri, position);

    const locations: XrefLocation[] = [];
    for (const target of normalizeLocations(results ?? [])) {
        if (locations.length > maxLocations) {
            break;
        }
        const summary = locations.length < maxLocations ? await lineText(target.uri, target.range.start.line) : "";
        locations.push({ file: target.uri.fsPath, line: target.range.start.line + 1, summary });
    }

    return { locations };
}

async function documentDiagnostics(
    req: OpRequest,
): Promise<{ diagnostics: ReportedDiagnostic[]; analyzed: boolean }> {
    const doc = await document(req.buf);
    const reported = vscode.languages.getDiagnostics(doc.uri).map((diagnostic) => ({
        line: diagnostic.range.start.line + 1,
        column: diagnostic.range.start.character + 1,
        severity: severityNames[diagnostic.severity],
        message: diagnostic.message,
    }));

    // An empty list has two readings: the document is clean, or no language
    // server has looked at it yet — the likely case just after open_buffer,
    // which loads a document no editor shows. Only a problem in hand or a
    // diagnostics event seen for this uri proves an analysis happened, and the
    // daemon words its answer accordingly.
    return {
        diagnostics: reported,
        analyzed: reported.length > 0 || analyzedDocuments.has(doc.uri.toString()),
    };
}

/**
 * Puts a question to the user and reports their pick.
 *
 * Concurrent questions are serialized: quick picks cannot be stacked, since
 * showing one hides any other, whose question would then be answered by
 * silence. Each question therefore waits for the previous one to settle.
 */
function askUser(req: OpRequest): Promise<{ answer: string }> {
    const question = req.prompt ?? "";
    const choices = req.choices ?? [];
    if (choices.length === 0) {
        throw new Error("ask requires at least one choice");
    }

    const answer = askQueue.then(
        () => pickOne(question, choices),
        () => pickOne(question, choices),
    );
    askQueue = answer.catch(() => undefined);

    return answer.then((picked) => ({ answer: picked }));
}

/**
 * Shows one question and resolves with the choice the user picks.
 *
 * Rejects when the picker is hidden without a pick, which the daemon reports as
 * a tool error: inventing an answer is precisely what ask exists to avoid.
 * Losing the focus is not a dismissal (ignoreFocusOut), and the question stays
 * on the status bar item while it is pending, which shows it again on click, so
 * a picker some other UI took the screen from is recoverable.
 */
function pickOne(question: string, choices: string[]): Promise<string> {
    return new Promise((resolve, reject) => {
        const picker = vscode.window.createQuickPick();
        picker.title = "sidekick";
        picker.placeholder = question;
        picker.items = choices.map((label) => ({ label }));
        picker.ignoreFocusOut = true;

        let picked: string | undefined;
        picker.onDidAccept(() => {
            picked = picker.selectedItems[0]?.label;
            picker.hide();
        });
        picker.onDidHide(() => {
            picker.dispose();
            pendingQuestion = undefined;
            refreshStatusBar();
            if (picked === undefined) {
                reject(new Error("user dismissed the question"));
            } else {
                resolve(picked);
            }
        });

        pendingQuestion = { question, show: () => picker.show() };
        refreshStatusBar();
        picker.show();
    });
}

// Buffers.

/**
 * The session's opened buffers, as absolute paths: the file-backed editor tabs
 * the user sees, plus the documents open_buffer loaded.
 *
 * Neither half is enough on its own. Tabs are what the user calls "open", but
 * open_buffer deliberately opens no tab, and dropping its documents would make
 * it useless for widening the perimeter of glob and grep;
 * workspace.textDocuments, the other candidate, also lists documents the user
 * never opened.
 */
function openBufferPaths(): string[] {
    const paths = new Set<string>();

    for (const group of vscode.window.tabGroups.all) {
        for (const tab of group.tabs) {
            const input: unknown = tab.input;
            if (input instanceof vscode.TabInputText && input.uri.scheme === "file") {
                paths.add(input.uri.fsPath);
            }
        }
    }
    for (const opened of openedByClaude) {
        paths.add(opened);
    }

    return [...paths].sort();
}

/**
 * Resolves a buffer id to its document.
 *
 * openTextDocument is idempotent and shows nothing, so a document VSCode has
 * disposed of since open_buffer loaded it silently comes back here.
 */
async function document(buf: string | undefined): Promise<vscode.TextDocument> {
    if (buf === undefined || buf === "") {
        throw new Error("no buffer given");
    }

    const root = requireSession().root;
    const uri = path.isAbsolute(buf) ? vscode.Uri.file(buf) : vscode.Uri.joinPath(root, buf);

    return vscode.workspace.openTextDocument(uri);
}

/** The buffer's lines, without their end-of-line characters. */
function documentLines(doc: vscode.TextDocument): string[] {
    const lines: string[] = [];
    for (let line = 0; line < doc.lineCount; line++) {
        lines.push(doc.lineAt(line).text);
    }
    return lines;
}

/**
 * The single range edit that replaces the lines [start, stop) of doc with
 * content, both 0-based and stop exclusive.
 *
 * A range ending inside the document ends on a line break the replacement then
 * carries; a range reaching the last line has none, so it takes the preceding
 * one instead. Getting this wrong leaves a stray blank line behind. The breaks
 * written are the document's own, so editing a CRLF file does not leave mixed
 * line endings.
 */
function lineEdit(
    doc: vscode.TextDocument,
    start: number,
    stop: number,
    content: string[],
): { range: vscode.Range; text: string } {
    const lastLine = doc.lineCount - 1;
    const eol = doc.eol === vscode.EndOfLine.CRLF ? "\r\n" : "\n";
    const text = content.join(eol);

    if (stop <= lastLine) {
        return {
            range: new vscode.Range(new vscode.Position(start, 0), new vscode.Position(stop, 0)),
            text: content.length === 0 ? "" : `${text}${eol}`,
        };
    }

    const documentEnd = doc.lineAt(lastLine).range.end;
    if (start > lastLine) {
        // Appending past the last line: each new line brings its own break.
        return {
            range: new vscode.Range(documentEnd, documentEnd),
            text: content.length === 0 ? "" : `${eol}${text}`,
        };
    }
    if (content.length === 0 && start > 0) {
        // Deleting through the last line: the break in front of the range goes
        // too, or line start survives as an empty line.
        return { range: new vscode.Range(doc.lineAt(start - 1).range.end, documentEnd), text: "" };
    }

    return { range: new vscode.Range(new vscode.Position(start, 0), documentEnd), text };
}

/** Converts a 1-based line and column into a position inside doc. */
function positionAt(doc: vscode.TextDocument, line: number, column: number): vscode.Position {
    return doc.validatePosition(new vscode.Position(Math.max(line - 1, 0), Math.max(column - 1, 0)));
}

/**
 * Flattens the two shapes a definition provider answers with: a Location holds
 * uri and range, a LocationLink targetUri and targetRange. Reference providers
 * only ever answer Locations, but they come through here too so that the rest
 * of the code sees one shape.
 */
function normalizeLocations(
    results: (vscode.Location | vscode.LocationLink)[],
): { uri: vscode.Uri; range: vscode.Range }[] {
    return results.map((result) =>
        "targetUri" in result
            ? { uri: result.targetUri, range: result.targetRange }
            : { uri: result.uri, range: result.range },
    );
}

/** The text of one line of a file, as the summary of a location. */
async function lineText(uri: vscode.Uri, line: number): Promise<string> {
    try {
        const doc = await vscode.workspace.openTextDocument(uri);
        if (line < doc.lineCount) {
            return doc.lineAt(line).text.trim();
        }
    } catch {
        // A definition VSCode cannot open as text (an archive, a generated
        // stub) still has a path and a line worth reporting.
    }
    return "";
}

/** The id the tools know a file by: its path relative to the session root, or
 * the absolute path when the file lives outside. Mirrors VSCodeMCPServer.rel. */
function bufferId(root: vscode.Uri, file: string): string {
    const relative = path.relative(root.fsPath, file);
    // Same rules as the daemon's rel(): only a leading ".." segment escapes the
    // root, so a file named "..config" keeps its id.
    if (
        relative === "" ||
        relative === ".." ||
        relative.startsWith(`..${path.sep}`) ||
        path.isAbsolute(relative)
    ) {
        return file;
    }
    // Slash-separated whatever the host separator is, since this id is what
    // glob patterns are matched against.
    return relative.split(path.sep).join("/");
}

// Daemon calls.

/**
 * Sends one JSON-RPC request to the daemon and resolves with its result.
 *
 * Rejects on a transport failure and on a JSON-RPC error, which is what lets
 * the caller condition a session start on a successful register — unlike the
 * nvim plugin, which fires its curl and forgets.
 */
async function rpcRequest(serverUrl: string, method: string, params: unknown): Promise<unknown> {
    nextRequestId += 1;
    const body = await postJson(serverUrl, { jsonrpc: "2.0", id: nextRequestId, method, params });
    if (body.trim() === "") {
        return undefined;
    }

    const answer = JSON.parse(body) as { result?: unknown; error?: { message?: string } };
    if (answer.error !== undefined) {
        throw new Error(answer.error.message ?? "unknown error");
    }
    return answer.result;
}

function postJson(url: string, payload: unknown): Promise<string> {
    return new Promise((resolve, reject) => {
        const target = new URL(url);
        const transport = target.protocol === "https:" ? https : http;
        const body = Buffer.from(JSON.stringify(payload), "utf8");

        const req = transport.request(
            target,
            {
                method: "POST",
                headers: { "Content-Type": "application/json", "Content-Length": body.byteLength },
            },
            (res) => {
                const chunks: Buffer[] = [];
                res.on("data", (chunk: Buffer) => chunks.push(chunk));
                res.on("end", () => {
                    const text = Buffer.concat(chunks).toString("utf8");
                    const status = res.statusCode ?? 0;
                    if (status >= 400) {
                        reject(new Error(`HTTP ${status}: ${text.trim()}`));
                    } else {
                        resolve(text);
                    }
                });
            },
        );
        req.on("error", reject);
        req.end(body);
    });
}

// Helpers.

/** The running session. Operations only ever arrive after a register. */
function requireSession(): Session {
    if (session === undefined) {
        throw new Error("no sidekick session in this window");
    }
    return session;
}

function clamp(value: number, low: number, high: number): number {
    return Math.min(Math.max(value, low), high);
}

function delay(ms: number): Promise<void> {
    return new Promise((resolve) => setTimeout(resolve, ms));
}

function describe(err: unknown): string {
    return err instanceof Error ? err.message : String(err);
}

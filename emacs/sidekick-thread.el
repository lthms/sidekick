;;; sidekick-thread.el --- Virtual REQ/ACK/RESP threads over a buffer -*- lexical-binding: t; -*-

;; Maintainer: Sylvain Ribstein <sylvain.ribstein@gmail.com>

;; This Source Code Form is subject to the terms of the Mozilla Public
;; License, v. 2.0. If a copy of the MPL was not distributed with this
;; file, You can obtain one at https://mozilla.org/MPL/2.0/.

;; A thread is a request to claude attached to a range of lines, drawn above
;; them but never written into the buffer: it is not saved, not part of undo
;; and never shows up in a diff.  It holds the user's `REQ' and claude's
;; `ACK'/`RESP' replies, in order.
;;
;; Each thread is an overlay spanning its lines.  The overlay's markers make
;; it follow the code as the buffer is edited; its `before-string' renders
;; the entries as virtual lines above the first one.  Threads live as long as
;; their buffer: killing or reverting it drops them, which
;; `sidekick-thread-materialize' avoids by turning a thread into comments.
;;
;; Claude reads and answers threads through the server's thread tools, which
;; land in `sidekick-thread--describe-all', `sidekick-thread--reply' and
;; `sidekick-thread--new' via `sidekick--dispatch'.

;;; Code:

(require 'newcomment)
(require 'pulse)
(require 'seq)
(require 'subr-x)

(declare-function sidekick-notify "sidekick" (&optional context thread))
(declare-function sidekick--annotation-face "sidekick" (tag))
(declare-function sidekick--buffer "sidekick" (name))

(defcustom sidekick-thread-notify t
  "Non-nil to notify claude as soon as a request is added to a thread.
When nil, notify it yourself with `sidekick-notify'."
  :type 'boolean
  :group 'sidekick)

(defvar sidekick-thread--next-id 1
  "Id given to the next thread; ids are unique across buffers.")

(defvar-local sidekick-thread--collapsed nil
  "Non-nil when this buffer's threads show only their first line.")

(defconst sidekick-thread--kinds '("REQ" "ACK" "RESP")
  "Kinds of thread entries: the user's requests, claude's replies.")

;;; Threads as overlays --------------------------------------------------------

(defun sidekick-thread--overlays (&optional buffer)
  "BUFFER's threads (default: the current buffer's), in buffer order."
  (with-current-buffer (or buffer (current-buffer))
    (sort (seq-filter (lambda (ov) (overlay-get ov 'sidekick-thread))
                      (overlays-in (point-min) (point-max)))
          (lambda (a b) (< (overlay-start a) (overlay-start b))))))

(defun sidekick-thread--all ()
  "Every thread of every live buffer."
  (seq-mapcat #'sidekick-thread--overlays
              (seq-filter (lambda (b) (not (string-prefix-p " " (buffer-name b))))
                          (buffer-list))))

(defun sidekick-thread--find (id)
  "The thread numbered ID, or an error."
  (or (seq-find (lambda (ov) (eql (overlay-get ov 'sidekick-thread) id))
                (sidekick-thread--all))
      (error "no thread #%s" id)))

(defun sidekick-thread--at-point ()
  "The innermost thread whose lines include point, or nil."
  (car (last (seq-filter
              (lambda (ov)
                (<= (save-excursion (goto-char (overlay-start ov))
                                    (line-beginning-position))
                    (point)
                    (save-excursion (goto-char (overlay-end ov))
                                    (line-end-position))))
              (sidekick-thread--overlays)))))

(defun sidekick-thread--make (beg end)
  "A new, empty thread over the lines from BEG to END."
  (let* ((start (save-excursion (goto-char beg) (line-beginning-position)))
         (stop (save-excursion
                 (goto-char (max start end))
                 ;; A region ending at the start of a line does not cover it.
                 (when (and (bolp) (> (point) start)) (backward-char))
                 (line-end-position)))
         (ov (make-overlay start stop nil nil nil)))
    (overlay-put ov 'sidekick-thread sidekick-thread--next-id)
    (overlay-put ov 'sidekick-thread-entries nil)
    (setq sidekick-thread--next-id (1+ sidekick-thread--next-id))
    ov))

(defun sidekick-thread--entries (ov)
  "OV's entries, a list of (KIND . TEXT) oldest first."
  (overlay-get ov 'sidekick-thread-entries))

(defun sidekick-thread--add-entry (ov kind text)
  "Append a KIND entry saying TEXT to thread OV and redraw it."
  (unless (member kind sidekick-thread--kinds)
    (error "unknown thread entry kind %S (one of %s)" kind
           (string-join sidekick-thread--kinds ", ")))
  (when (string-blank-p (or text ""))
    (error "empty %s entry" kind))
  (overlay-put ov 'sidekick-thread-entries
               (append (sidekick-thread--entries ov)
                       (list (cons kind (string-trim text)))))
  (sidekick-thread--render ov))

(defun sidekick-thread--status (ov)
  "OV's status: \"open\" while its last entry is an unanswered request."
  (pcase (car (car (last (sidekick-thread--entries ov))))
    ("REQ" "open")
    ("ACK" "acknowledged")
    (_ "answered")))

;;; Rendering ------------------------------------------------------------------

(defun sidekick-thread--entry-string (kind text indent &optional suffix)
  "Virtual lines showing a KIND entry saying TEXT, indented by INDENT.
SUFFIX, when non-nil, ends the entry's first line."
  (let* ((face (sidekick--annotation-face kind))
         (tag (concat kind ": "))
         (pad (make-string (+ indent (length tag)) ?\s))
         (lines (split-string text "\n")))
    (concat
     (propertize (make-string indent ?\s) 'face face)
     (propertize tag 'face (list 'bold face))
     (propertize (car lines) 'face face)
     (or suffix "")
     (propertize "\n" 'face face)
     (mapconcat (lambda (l) (propertize (concat pad l "\n") 'face face))
                (cdr lines) ""))))

(defun sidekick-thread--render (ov)
  "Redraw thread OV's entries as virtual lines above its first line."
  (let* ((entries (sidekick-thread--entries ov))
         (indent (with-current-buffer (overlay-buffer ov)
                   (save-excursion (goto-char (overlay-start ov))
                                   (current-indentation))))
         (id (propertize (format "  #%d" (overlay-get ov 'sidekick-thread))
                         'face 'shadow)))
    (overlay-put
     ov 'before-string
     (when entries
       (if (buffer-local-value 'sidekick-thread--collapsed (overlay-buffer ov))
           (let ((first (car entries)))
             (sidekick-thread--entry-string
              (car first)
              (truncate-string-to-width
               (car (split-string (cdr first) "\n")) 60 nil nil "…")
              indent
              (concat (propertize (format "  [%s, %d %s]"
                                          (sidekick-thread--status ov)
                                          (length entries)
                                          (if (cdr entries) "entries" "entry"))
                                  'face 'shadow)
                      id)))
         (let ((first t))
           (mapconcat (lambda (e)
                        (prog1 (sidekick-thread--entry-string
                                (car e) (cdr e) indent (and first id))
                          (setq first nil)))
                      entries "")))))))

(defun sidekick-thread--reveal (ov)
  "Pulse thread OV's first line in every window showing its buffer."
  (let ((buffer (overlay-buffer ov))
        (pos (overlay-start ov)))
    (dolist (win (get-buffer-window-list buffer nil t))
      (with-selected-window win
        (when (pos-visible-in-window-p pos win)
          (pulse-momentary-highlight-one-line pos))))))

;;; Server-facing operations ---------------------------------------------------

(defun sidekick-thread--describe (ov)
  "Thread OV as an alist for the server."
  (with-current-buffer (overlay-buffer ov)
    (save-restriction
      (widen)
      `((id . ,(overlay-get ov 'sidekick-thread))
        (buf . ,(buffer-name))
        (file . ,(or (buffer-file-name) ""))
        (line . ,(line-number-at-pos (overlay-start ov) t))
        (endLine . ,(line-number-at-pos (overlay-end ov) t))
        (status . ,(sidekick-thread--status ov))
        (entries . ,(vconcat
                     (mapcar (lambda (e) `((kind . ,(car e)) (text . ,(cdr e))))
                             (sidekick-thread--entries ov))))))))

(defun sidekick-thread--describe-all ()
  "Every thread as a vector of alists for the server."
  (vconcat (mapcar #'sidekick-thread--describe (sidekick-thread--all))))

(defun sidekick-thread--reply (id kind text)
  "Add claude's KIND (\"ACK\" or \"RESP\") reply saying TEXT to thread ID."
  (unless (member kind '("ACK" "RESP"))
    (error "a reply is an ACK or a RESP, not %S" kind))
  (let ((ov (sidekick-thread--find id)))
    (sidekick-thread--add-entry ov kind text)
    (sidekick-thread--reveal ov)
    (message "sidekick: claude replied on thread #%d (%s)"
             id (buffer-name (overlay-buffer ov)))))

(defun sidekick-thread--new (buf line end-line text)
  "Open a thread on BUF's lines LINE to END-LINE (1-based) with claude's TEXT.
END-LINE of 0 means LINE alone.  Returns the new thread's id."
  (with-current-buffer (sidekick--buffer buf)
    (save-restriction
      (widen)
      (let* ((bol (lambda (n) (save-excursion (goto-char (point-min))
                                              (forward-line (1- n))
                                              (point))))
             (last (if (and (natnump end-line) (> end-line 0)) end-line line))
             (ov (sidekick-thread--make
                  (funcall bol line)
                  (save-excursion (goto-char (funcall bol last))
                                  (line-end-position)))))
        (sidekick-thread--add-entry ov "RESP" text)
        (sidekick-thread--reveal ov)
        (message "sidekick: claude opened thread #%d in %s"
                 (overlay-get ov 'sidekick-thread) buf)
        (overlay-get ov 'sidekick-thread)))))

;;; Commands -------------------------------------------------------------------

(defun sidekick-thread--read-thread (prompt)
  "The thread at point, or one picked with PROMPT among the buffer's."
  (or (sidekick-thread--at-point)
      (let* ((threads (or (sidekick-thread--overlays)
                          (user-error "No sidekick thread in this buffer")))
             (choices (mapcar (lambda (ov)
                                (cons (format "#%d line %d: %s"
                                              (overlay-get ov 'sidekick-thread)
                                              (line-number-at-pos (overlay-start ov))
                                              (cdr (car (sidekick-thread--entries ov))))
                                      ov))
                              threads)))
        (cdr (assoc (completing-read prompt choices nil t) choices)))))

;;;###autoload
(defun sidekick-thread-add (text)
  "Ask claude TEXT in a thread drawn above the code, not written into it.
With an active region, opens a new thread over its lines.  Otherwise, on a
line a thread covers, adds TEXT to it as a follow-up request; elsewhere,
opens a new thread over the current line.  Then notifies claude, unless
`sidekick-thread-notify' is nil."
  (interactive (list (read-string (if (and (not (use-region-p))
                                           (sidekick-thread--at-point))
                                      "Follow-up request: "
                                    "Request: "))))
  (let ((ov (if (use-region-p)
                (sidekick-thread--make (region-beginning) (region-end))
              (or (sidekick-thread--at-point)
                  (sidekick-thread--make (point) (point))))))
    (sidekick-thread--add-entry ov "REQ" text)
    (deactivate-mark)
    (when sidekick-thread-notify
      (sidekick-notify nil (overlay-get ov 'sidekick-thread)))))

;;;###autoload
(defun sidekick-thread-resolve (thread)
  "Delete THREAD (the one at point, or one picked), once it is settled."
  (interactive (list (sidekick-thread--read-thread "Resolve thread: ")))
  (let ((id (overlay-get thread 'sidekick-thread)))
    (delete-overlay thread)
    (message "sidekick: resolved thread #%d" id)))

;;;###autoload
(defun sidekick-thread-next (&optional n)
  "Move to the first line of the Nth next thread (previous when N < 0)."
  (interactive "p")
  (let* ((n (or n 1))
         (here (line-beginning-position))
         (starts (mapcar #'overlay-start (sidekick-thread--overlays)))
         (candidates (if (> n 0)
                         (seq-filter (lambda (p) (> p here)) starts)
                       (reverse (seq-filter (lambda (p) (< p here)) starts))))
         (target (nth (1- (abs n)) candidates)))
    (if target
        (goto-char target)
      (user-error "No %s sidekick thread" (if (> n 0) "next" "previous")))))

;;;###autoload
(defun sidekick-thread-previous (&optional n)
  "Move to the first line of the Nth previous thread."
  (interactive "p")
  (sidekick-thread-next (- (or n 1))))

;;;###autoload
(defun sidekick-thread-toggle ()
  "Collapse this buffer's threads to one line each, or expand them back."
  (interactive)
  (setq sidekick-thread--collapsed (not sidekick-thread--collapsed))
  (mapc #'sidekick-thread--render (sidekick-thread--overlays))
  (message "sidekick: threads %s"
           (if sidekick-thread--collapsed "collapsed" "expanded")))

;;;###autoload
(defun sidekick-thread-list ()
  "Pick any buffer's thread and jump to it."
  (interactive)
  (let* ((threads (or (sidekick-thread--all)
                      (user-error "No sidekick thread")))
         (choices
          (mapcar (lambda (ov)
                    (cons (format "#%d %s:%d [%s] %s"
                                  (overlay-get ov 'sidekick-thread)
                                  (buffer-name (overlay-buffer ov))
                                  (with-current-buffer (overlay-buffer ov)
                                    (line-number-at-pos (overlay-start ov)))
                                  (sidekick-thread--status ov)
                                  (cdr (car (sidekick-thread--entries ov))))
                          ov))
                  threads))
         (ov (cdr (assoc (completing-read "Thread: " choices nil t) choices))))
    (pop-to-buffer-same-window (overlay-buffer ov))
    (goto-char (overlay-start ov))))

;;;###autoload
(defun sidekick-thread-materialize (thread)
  "Write THREAD into the buffer as REQ:/ACK:/RESP: comments, then drop it.
Use it to keep a thread in the file -- threads otherwise vanish with
their buffer."
  (interactive (list (sidekick-thread--read-thread "Materialize thread: ")))
  (with-current-buffer (overlay-buffer thread)
    (save-excursion
      (goto-char (overlay-start thread))
      (beginning-of-line)
      (let ((indent (current-indentation))
            (beg (point)))
        (dolist (e (sidekick-thread--entries thread))
          (let ((tag (concat (car e) ": ")))
            (insert (make-string indent ?\s) tag
                    (replace-regexp-in-string
                     "\n"
                     (concat "\n" (make-string (+ indent (length tag)) ?\s))
                     (cdr e))
                    "\n")))
        (comment-normalize-vars t)
        (when comment-start
          (comment-region beg (point)))))
    (delete-overlay thread)))

(provide 'sidekick-thread)

;;; sidekick-thread.el ends here

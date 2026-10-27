/**
 * Fixed geometry of a workflow card.
 *
 * These numbers are shared by three consumers that must agree or the canvas
 * drifts: the card renders at this width, the palette centers a new node with
 * them, and the connection handles sit at the anchor height. React Flow measures
 * the real DOM box after the first paint, so these are the values used before
 * that measurement exists.
 */

/** Rendered width of every workflow card. */
export const WORKFLOW_NODE_WIDTH = 230

/** Height React Flow assumes for a card until the browser measures the real one. */
export const WORKFLOW_NODE_INITIAL_HEIGHT = 98

/** Vertical offset of the connection handles from the top of a card. */
export const WORKFLOW_NODE_ANCHOR_Y = 61

/** Hit area of one connection handle, in pixels. */
export const WORKFLOW_NODE_HANDLE_SIZE = 10

/**
 * Size an Iteration frame takes when it is expanded around its members.
 *
 * The frame is authored rather than measured: a region with no members still has
 * to be droppable, and the size has to survive a save/load round trip, so it is
 * persisted on the node as `initialWidth` / `initialHeight`.
 */
export const WORKFLOW_ITERATION_NODE_WIDTH = 560

/** Height of an expanded Iteration frame. See {@link WORKFLOW_ITERATION_NODE_WIDTH}. */
export const WORKFLOW_ITERATION_NODE_HEIGHT = 340

/** Width of an Iteration frame folded to its summary bar. */
export const WORKFLOW_ITERATION_COLLAPSED_WIDTH = 320

/** Height of an Iteration frame folded to its summary bar. */
export const WORKFLOW_ITERATION_COLLAPSED_HEIGHT = 56

/** Horizontal inset of a region member from its frame's left edge. */
export const WORKFLOW_ITERATION_MEMBER_LEFT = 120

/** Vertical inset of a region member from its frame's top edge. */
export const WORKFLOW_ITERATION_MEMBER_TOP = 100

/**
 * Vertical offset of the frame's entry handle.
 *
 * The entry edge leaves the frame at card anchor height inside the member band,
 * so the wire from the frame to its first member is a straight horizontal run.
 */
export const WORKFLOW_ITERATION_ENTRY_ANCHOR_Y =
  WORKFLOW_ITERATION_MEMBER_TOP + WORKFLOW_NODE_ANCHOR_Y

/**
 * Authored size of an expanded Loop frame.
 *
 * Like the Iteration frame, the Loop size is authored rather than measured so the
 * region survives a save/load round trip and stays droppable while empty.
 */
export const WORKFLOW_LOOP_NODE_WIDTH = 620

/** Height of an expanded Loop frame. See {@link WORKFLOW_LOOP_NODE_WIDTH}. */
export const WORKFLOW_LOOP_NODE_HEIGHT = 300

/**
 * Smallest box a manual resize may leave a Loop frame at: room for the body start and one
 * card. Matches desktop, so a graph resized there imports without being clamped here.
 */
export const WORKFLOW_LOOP_MIN_WIDTH = 480

/** Smallest Loop frame height. See {@link WORKFLOW_LOOP_MIN_WIDTH}. */
export const WORKFLOW_LOOP_MIN_HEIGHT = 260

/** Width a Condition card assumes until the browser measures the real one. */
export const WORKFLOW_CONDITION_NODE_WIDTH = 320

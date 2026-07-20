// Pure reducer for the durable event cursor.
//
// It is separated from the page so the gap and duplicate rules can be tested
// directly, without needing to make a browser drop a specific frame. The rules
// are the whole of the client's trust model for the stream:
//
//   - an event at exactly cursor+1 continues the sequence and may be applied;
//   - an event at or below the cursor was already seen and is ignored, which is
//     what makes a Last-Event-ID replay harmless;
//   - anything beyond cursor+1 means this client missed something, so it must
//     re-read the authoritative snapshot rather than guess at the gap.
//
// The reducer never invents state. "apply" only means the cursor advanced and
// the event may be recorded in the timeline; what the board looks like always
// comes from a server snapshot.

export const APPLY = "apply";
export const IGNORE = "ignore";
export const RESYNC = "resync";

/**
 * @param {{cursor: number}} state
 * @param {{sequence: number}} event
 * @returns {{cursor: number, action: string}}
 */
export function reduceStream(state, event) {
  const cursor = Number(state && state.cursor) || 0;
  const sequence = Number(event && event.sequence);
  if (!Number.isFinite(sequence) || sequence <= 0) {
    // A frame without a usable sequence cannot be placed in the order, so it
    // is treated as a gap rather than silently dropped.
    return { cursor, action: RESYNC };
  }
  if (sequence <= cursor) {
    return { cursor, action: IGNORE };
  }
  if (sequence > cursor + 1) {
    return { cursor, action: RESYNC };
  }
  return { cursor: sequence, action: APPLY };
}

/**
 * boundedTimeline keeps the activity list from growing without limit.
 * @param {Array} timeline
 * @param {Object} entry
 * @param {number} limit
 */
export function boundedTimeline(timeline, entry, limit = 500) {
  const next = timeline.concat([entry]);
  return next.length > limit ? next.slice(next.length - limit) : next;
}

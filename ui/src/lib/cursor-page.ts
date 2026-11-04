export interface CursorPageState {
  cursor?: string
  previous: Array<string | undefined>
}

export function firstCursorPage(): CursorPageState {
  return { cursor: undefined, previous: [] }
}

export function nextCursorPage(state: CursorPageState, nextCursor?: string): CursorPageState {
  if (!nextCursor) return state
  return { cursor: nextCursor, previous: [...state.previous, state.cursor] }
}

export function previousCursorPage(state: CursorPageState): CursorPageState {
  if (state.previous.length === 0) return state
  const previous = state.previous.slice(0, -1)
  return { cursor: state.previous[state.previous.length - 1], previous }
}

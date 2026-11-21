/** Leaving the app is its own seam so tests can see where the approval sends the browser. */
export const navigation = { assign: (url: string) => window.location.assign(url) }

function getOrigin() {
	url = window.location.href;

	let parsedUrl;
	try {
		parsedUrl = new URL(url);
	} catch (error) {
		console.error("Invalid URL:", error);
		return null;
	}

	// Extract and return the hostname (domain)
	return parsedUrl.origin;
}

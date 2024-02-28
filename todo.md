# Todo

## In no particular order

- Completely rewrite the server & streaming logic
- Grab client meta data
    - IP, user agent etc
    - Do we get any signal when the client disconnects?
- Handle signals like SIGINT elegantly - maybe play a little "shutting down" mp3!
    - [Yes!](https://www.youtube.com/watch?v=Gb2jGy76v0Y)
- Pass in a podcast XML file or URL
    - Randomise or go in order
    - Download files only once
- Use [mp3snip](https://github.com/sweeney/mp3snip) to snip the ads at the start/end of a podcast; some config required for different podcasts?

# Sitkin

Sitkin is yet another static blog engine written in Go.

I wrote this purely to satisfy my own needs for my own website.

## Project layout for Sitkin

Here's an example:

```
├── sitkin
│   ├── config.json
│   ├── default.tmpl
│   └── posts.tmpl
├── posts
│   └── 2018-03-05.hello-world.md
├── gen
├── assets
│   ├── css
│   ├── images
│   └── js
├── favicon.ico
├── index.tmpl
└── about.md
```

config.json contains:

```
{
  "ignore": [],
  "nohash": ["favicon.ico"],
  "filesets": ["posts"]
}
```

* sitkin/config.json contains a few configuration options:
  - `ignore` is a list of file globs relative to the top level directory to
    ignore when generating the result site.
  - `nohash` is a list of file globs for asset files that should *not* be
    renamed with a hash of their contents.
  - `filesets` is a list of the file sets (see below).
* The `sitkin` directory contains templates that are used to render other files.
  - `default.tmpl` is the default template that renders every page.
  - `posts.tmpl`, in this example, is the template for the posts directory.
  - When rendering posts (in this example), the template set contains
    `default.tmpl` and `posts.tmpl`. When rendering other pages, the template
    set contains only `default.tmpl`.
* The posts directory is a *file set* of Markdown files. Sitkin knows that posts
  should be rendered (rather than just copied directly) because `posts` is
  listed as a fileset in config.json.
  - Each markdown file can include metadata, which is arbitrary JSON text
    accessible from the template, at the beginning of the file delimited by an
    HTML comment (`<!--` and `-->`).
* The `gen` directory contains the generated files. (It should be gitignored.)
* Other directories, like `assets` in this example, are directly copied as-is.
* Templates like `index.tmpl` and markdown files are rendered to html files.

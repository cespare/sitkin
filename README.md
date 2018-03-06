# Sitkin

Sitkin is yet another static blog engine written in Go.

I wrote this purely to satisfy my own needs for my own website.

## Project layout for Sitkin

Here's an example:

```
├── sitkin
│   ├── default.tmpl
│   └── posts.tmpl
├── posts
│   └── 2018-03-05.hello-world.md
├── gen
├── assets
│   ├── css
│   ├── images
│   └── js
├── index.tmpl
└── about.md
```

* The `sitkin` directory contains templates that are used to render other files.
  - `default.tmpl` is the default template that renders every page.
  - `posts.tmpl`, in this example, is the template for the posts directory.
  - When rendering posts (in this example), the template set contains
    `default.tmpl` and `posts.tmpl`. When rendering other pages, the template
    set contains only `default.tmpl`.
* The posts directory is a *file set* of Markdown files. In this case Sitkin
  knows that posts should be rendered (rather than just copied directly) because
  of `sitkin/posts.tmpl`.
  - Each markdown file can include metadata, which is arbitrary JSON text
    accessible from the template, at the beginning of the file delimited by an
    HTML comment (`<!--` and `-->`).
* The `gen` directory contains the generated files. (It should be gitignored.)
* Other directories, like `assets` in this example, are directly copied as-is.
* Templates like `index.tmpl` and markdown files are rendered to html files.

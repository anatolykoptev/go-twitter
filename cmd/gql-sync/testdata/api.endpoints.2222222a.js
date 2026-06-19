"use strict";
// Trimmed GraphQL operation registry. Exercises BOTH op-definition shapes the
// extractor handles. Each op object stays on one line (the regexes' `.+?` does
// not span newlines), mirroring a real minified bundle.
//
// Shape 1 — { queryId, operationName } (opRe):
var ops = [
{queryId:"IGgvgiOx4QZndDHuD3x9TQ",operationName:"UserByScreenName",metadata:{}},
{queryId:"GcXk9vN_d1jUfHNqLacXQA",operationName:"SearchTimeline",metadata:{}}
];
// Shape 2 — params:{ id, name, … operationKind } persisted queries (paramsRe):
e.exports = {
n0:{params:{id:"VWFGPVAGkZMGRKGe3GFFnA",metadata:{},name:"TweetDetail",operationKind:"query"}},
n1:{params:{id:"7TKRKCPuAGsmYde0CudbVg",metadata:{},name:"CreateTweet",operationKind:"mutation"}}
};

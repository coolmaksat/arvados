cwlVersion: v1.0
class: Workflow
inputs: []
outputs: []
$namespaces:
  arv: "http://arvados.org/cwl#"
requirements:
  SubworkflowFeatureRequirement: {}
steps:
  step1:
    requirements:
      arv:RunInSingleContainer: {}
    in: []
    out: []
    run: default-dir7.cwl